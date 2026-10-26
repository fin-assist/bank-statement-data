package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

var accountType = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Stage links source keys and unread facts, before the usecase applies accounting
// and completes the same transaction. A failure rolls back every inserted fact.
func (r *Imports) Stage(ctx context.Context, user, id string, document importing.Document, _ time.Time) (importing.Stage, error) {
	if _, err := r.transaction(ctx); err != nil {
		return importing.Stage{}, err
	}
	accounts := make(map[string]int64)
	var ids []int64
	for _, account := range document.Accounts {
		if account.Key == "" || !utf8.ValidString(account.Key) || utf8.RuneCountInString(account.Key) > 64 || strings.ContainsRune(account.Key, 0) || !accountType.MatchString(account.Type) {
			return importing.Stage{}, errImportStorage
		}
		if _, exists := accounts[account.Key]; exists {
			continue
		}
		var accountID int64
		err := r.db.Writer(ctx).QueryRow(ctx, `INSERT INTO account(user_id,source,ext_id,type,mask,is_other_bank) VALUES($1,'tbank_ofx',$2,$3,$4,$5) ON CONFLICT(user_id,source,ext_id) DO UPDATE SET type=EXCLUDED.type,mask=EXCLUDED.mask,is_other_bank=EXCLUDED.is_other_bank RETURNING id`, user, account.Key, account.Type, account.Mask, account.OtherBank).Scan(&accountID)
		if err != nil {
			return importing.Stage{}, importError(ctx, err)
		}
		accounts[account.Key] = accountID
		ids = append(ids, accountID)
	}
	newIDs := make(map[string]bool)
	categories := make(map[string]string)
	occurrences := make(map[int]int)
	for _, line := range document.Lines {
		if err := ctx.Err(); err != nil {
			return importing.Stage{}, err
		}
		accountID, exists := accounts[line.AccountKey]
		if !exists || line.Number <= 0 {
			return importing.Stage{}, errImportStorage
		}
		if line.Reason != "" {
			if line.Reason != "no_date" && line.Reason != "bad_date" && line.Reason != "bad_amount" && line.Reason != "truncated" || utf8.RuneCountInString(line.FITID) > 255 {
				return importing.Stage{}, errImportStorage
			}
			occurrences[line.Number]++
			_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO unread_line(user_id,upload_id,line_number,line_occurrence,reason,occurred_at,account_id,external_id) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, user, id, line.Number, occurrences[line.Number], line.Reason, line.OccurredAt, accountID, line.FITID)
			if err != nil {
				return importing.Stage{}, importError(ctx, err)
			}
			continue
		}
		if line.OccurredAt == nil || line.OccurredAt.IsZero() || line.AmountMinor < 0 || !currencyCode.MatchString(line.Currency) || line.FITID == "" || utf8.RuneCountInString(line.FITID) > 255 || line.Direction != "income" && line.Direction != "expense" {
			return importing.Stage{}, errImportStorage
		}
		var existing string
		err := r.db.Writer(ctx).QueryRow(ctx, `SELECT operation_id::text FROM source_record WHERE user_id=$1 AND account_id=$2 AND external_id=$3`, user, accountID, line.FITID).Scan(&existing)
		if err == nil {
			_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE source_record SET upload_ids=array_append(upload_ids,$4::uuid) WHERE user_id=$1 AND account_id=$2 AND external_id=$3 AND NOT($4::uuid=ANY(upload_ids))`, user, accountID, line.FITID, id)
			if err != nil {
				return importing.Stage{}, importError(ctx, err)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return importing.Stage{}, importError(ctx, err)
		}
		category, ok := categories[line.BankCategory]
		if !ok {
			category, err = r.bankCategory(ctx, line.BankCategory)
			if err != nil {
				return importing.Stage{}, err
			}
			categories[line.BankCategory] = category
		}
		var extensions []byte
		if line.BankCategory == "Возврат" {
			extensions = []byte(`{"tbank_ofx":{"bank_refund":true}}`)
		}
		var operationID string
		err = r.db.Writer(ctx).QueryRow(ctx, `INSERT INTO operation(user_id,occurred_at,account_id,direction,amount_minor,currency,status,description,bank_category_id,category_id,extensions) VALUES($1,$2,$3,$4,$5,$6,'posted',$7,$8,$8,$9::jsonb) RETURNING id::text`, user, *line.OccurredAt, accountID, line.Direction, line.AmountMinor, line.Currency, truncateText(line.Description, 1000), category, extensions).Scan(&operationID)
		if err != nil {
			return importing.Stage{}, importError(ctx, err)
		}
		_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO source_record(user_id,account_id,external_id,operation_occurred_at,operation_id,upload_ids) VALUES($1,$2,$3,$4,$5,ARRAY[$6::uuid])`, user, accountID, line.FITID, *line.OccurredAt, operationID, id)
		if err != nil {
			return importing.Stage{}, importError(ctx, err)
		}
		newIDs[operationID] = true
	}
	state, err := r.Snapshot(ctx, user)
	return importing.Stage{Snapshot: state, NewIDs: newIDs, AccountIDs: ids}, err
}

func (r *Imports) bankCategory(ctx context.Context, raw string) (string, error) {
	name := truncateText(raw, 64)
	if name == "" || name == "Без категории" || name == "Возврат" {
		return importing.UncategorizedCategory, nil
	}
	var id string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT id FROM category WHERE bank_name=$1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", importError(ctx, err)
	}
	base := categorySlug(name)
	hash := sha256.Sum256([]byte(name))
	for _, candidate := range []string{"cat_" + base, "cat_" + base + "_" + hex.EncodeToString(hash[:6])} {
		_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO category(id,name,bank_name,icon,for_expense,for_income,assignable) VALUES($1,$2,$2,'tag',true,true,true) ON CONFLICT DO NOTHING`, candidate, name)
		if err != nil {
			return "", importError(ctx, err)
		}
		err = r.db.Writer(ctx).QueryRow(ctx, `SELECT id FROM category WHERE bank_name=$1`, name).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", importError(ctx, err)
		}
	}
	return "", errImportStorage
}

func categorySlug(name string) string {
	const cyrillic = "абвгдеёжзийклмнопрстуфхцчшщъыьэюя"
	latin := []string{"a", "b", "v", "g", "d", "e", "e", "zh", "z", "i", "i", "k", "l", "m", "n", "o", "p", "r", "s", "t", "u", "f", "kh", "ts", "ch", "sh", "shch", "", "y", "", "e", "yu", "ya"}
	var slug strings.Builder
	for _, char := range strings.ToLower(name) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			slug.WriteRune(char)
			continue
		}
		matched := false
		for index, source := range []rune(cyrillic) {
			if source == char {
				slug.WriteString(latin[index])
				matched = true
				break
			}
		}
		if !matched && slug.Len() > 0 && !strings.HasSuffix(slug.String(), "_") {
			slug.WriteByte('_')
		}
	}
	result := strings.Trim(slug.String(), "_")
	if result == "" {
		result = "category"
	}
	if len(result) > 230 {
		result = strings.TrimRight(result[:230], "_")
	}
	return result
}

func truncateText(value string, limit int) string {
	characters := []rune(value)
	if len(characters) > limit {
		return string(characters[:limit])
	}
	return value
}
