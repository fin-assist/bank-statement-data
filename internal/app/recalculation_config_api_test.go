package app_test

import (
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func configuredRecalculationOffset(t *testing.T, raw string) time.Duration {
	t.Helper()
	for name, value := range map[string]string{"SERVICE_NAME": "bank-statement-data", "ENV": "dev", "AUTH_MODE": "insecure-dev", "DB_PRIMARY_DSN": "postgres://fixture/config", "OBJECT_STORAGE_BUCKET": "fixture", "OBJECT_STORAGE_KMS_KEY_ID": "fixture", "OBJECT_STORAGE_IAM_TOKEN_FILE": "/fixture/iam", "DAILY_RECALCULATION_OFFSET": raw} {
		t.Setenv(name, value)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DailyRecalculationOffset
}

func TestAPIDailyOffsetConfigWiresEverySchedulingPath(t *testing.T) {
	for _, tc := range []struct{ name, raw, wantMoscow, wantUTC string }{
		{"default", "", "2026-09-27T00:05:00+03:00", "2026-09-26T00:05:00Z"},
		{"ten minutes", "10m", "2026-09-26T00:10:00+03:00", "2026-09-26T00:10:00Z"},
		{"explicit zero", "0", "2026-09-27T00:00:00+03:00", "2026-09-26T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset := configuredRecalculationOffset(t, tc.raw)
			now := func() time.Time { return operationsTime(t, "2026-09-26T00:06:00+03:00").AsTime() }
			env := apitest.StartWith(t, app.Options{Now: now}, func(c *config.Config) { c.DailyRecalculationOffset = offset })
			ctx := apitest.User(t.Context(), apiUser)
			operationsAPIImport(t, env, calculationRuleOFX())
			assertDue := func(want string) {
				t.Helper()
				at, err := time.Parse(time.RFC3339, want)
				if err != nil {
					t.Fatal(err)
				}
				var got time.Time
				if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT daily_recalc_due_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&got); err != nil || !got.Equal(at) {
					t.Fatal("configured offset not used by real scheduling path", tc.raw, got, at, err)
				}
			}
			resetDue := func() {
				t.Helper()
				if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE user_state SET daily_recalc_due_at='2001-01-01'::timestamptz WHERE user_id=$1`, apiUser); err != nil {
					t.Fatal(err)
				}
			}
			assertDue(tc.wantMoscow)
			client := pb.NewOperationsServiceClient(env.Conn)
			feed, err := client.ListFeed(ctx, &pb.ListFeedRequest{})
			if err != nil {
				t.Fatal(err)
			}
			item := feed.Items[0]
			resetDue()
			if _, err = client.ChangeCategory(ctx, &pb.ChangeCategoryRequest{Id: item.Id, OccurredAt: item.OccurredAt, CategoryId: "cat_taxi"}); err != nil {
				t.Fatal(err)
			}
			assertDue(tc.wantMoscow)
			result, err := env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
			if err != nil {
				t.Fatal(err)
			}
			payment := result.Periods[0].Metrics["insights.regular_payments"].Payments[0].ID
			regular := pb.NewRegularPaymentsServiceClient(env.Conn)
			resetDue()
			if _, err = regular.DismissRegularPayment(ctx, &pb.DismissRegularPaymentRequest{RegularPaymentId: payment}); err != nil {
				t.Fatal(err)
			}
			assertDue(tc.wantMoscow)
			resetDue()
			if _, err = regular.RestoreRegularPayment(ctx, &pb.RestoreRegularPaymentRequest{RegularPaymentId: payment}); err != nil {
				t.Fatal(err)
			}
			assertDue(tc.wantMoscow)
			resetDue()
			if _, err = settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "UTC", SettingsVersion: 1}); err != nil {
				t.Fatal(err)
			}
			assertDue(tc.wantUTC)
			// Keep a second completed import so DeleteUpload must schedule remaining data.
			operationsAPIImport(t, env, calculationRuleOFX())
			var upload string
			if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT id::text FROM upload WHERE user_id=$1 AND status='done' ORDER BY uploaded_at,id LIMIT 1`, apiUser).Scan(&upload); err != nil {
				t.Fatal(err)
			}
			resetDue()
			if _, err = pb.NewStatementsServiceClient(env.Conn).DeleteUpload(ctx, &pb.DeleteUploadRequest{UploadId: upload}); err != nil {
				t.Fatal(err)
			}
			assertDue(tc.wantUTC)
		})
	}
}
