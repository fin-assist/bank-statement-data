package importing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	idem "github.com/fin-assist/go-idempotency"
	"github.com/google/uuid"
)

// MaxFileBytes is the contract's actual file limit, excluding multipart metadata.
const MaxFileBytes int64 = 10 << 20

// Service owns admission and durable import lifecycle, with no remote I/O in transactions.
type Service struct {
	tx          Transactions
	repo        Repository
	objects     ObjectStore
	barrier     ObjectBarrier
	parser      Parser
	keys        Keys
	deleted     Tombstones
	now         func() time.Time
	newID       func() (string, error)
	method      string
	slots       chan struct{}
	receiveMu   sync.Mutex
	receivers   map[string]*receivingUser
	sweepMu     sync.Mutex
	sweepCursor string
}

type receivingUser struct {
	slot chan struct{}
	refs int
}

// acquireReceiver prevents one user from occupying every receiving buffer.
// Waiters hold no global buffer; entries disappear when their last caller leaves.
func (s *Service) acquireReceiver(ctx context.Context, user string) (func(), error) {
	s.receiveMu.Lock()
	if s.receivers == nil {
		s.receivers = make(map[string]*receivingUser)
	}
	entry := s.receivers[user]
	if entry == nil {
		entry = &receivingUser{slot: make(chan struct{}, 1)}
		s.receivers[user] = entry
	}
	entry.refs++
	s.receiveMu.Unlock()
	leave := func() {
		s.receiveMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.receivers, user)
		}
		s.receiveMu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-entry.slot; leave() }) }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

// New wires explicit ports. Four admitted buffers bound process memory even when
// clients concurrently send the maximum-size file; waiting consumes no file buffer.
func New(tx Transactions, repo Repository, objects ObjectStore, barrier ObjectBarrier, parser Parser, keys Keys, deleted Tombstones, now func() time.Time, newID func() (string, error), method string) (*Service, error) {
	if tx == nil || repo == nil || objects == nil || barrier == nil || parser == nil || keys == nil || deleted == nil || now == nil || newID == nil || method == "" {
		return nil, errors.New("invalid import service dependencies")
	}
	return &Service{tx: tx, repo: repo, objects: objects, barrier: barrier, parser: parser, keys: keys, deleted: deleted, now: now, newID: newID, method: method, slots: make(chan struct{}, 4)}, nil
}

// Upload reads the actual bounded bytes before looking at the idempotency key.
// Oversize therefore never validates/reserves a key or creates an upload.
func (s *Service) Upload(ctx context.Context, user string, header Header, body io.Reader, rawKeys []string) (out Accepted, rpcErr error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if header.SizeBytes > MaxFileBytes {
		return Accepted{}, ErrTooLarge
	}
	if header.SizeBytes < 0 {
		return Accepted{}, &InputError{"header.size_bytes", "OUT_OF_RANGE"}
	}
	if !utf8.ValidString(header.FileName) || strings.ContainsRune(header.FileName, 0) {
		return Accepted{}, &InputError{"header.file_name", "INVALID_FORMAT"}
	}
	receiveDone, err := s.acquireReceiver(ctx, user)
	if err != nil {
		return Accepted{}, err
	}
	defer receiveDone()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Accepted{}, ctx.Err()
	}
	content, err := io.ReadAll(io.LimitReader(body, MaxFileBytes+1))
	receiveDone()
	if int64(len(content)) > MaxFileBytes {
		return Accepted{}, ErrTooLarge
	}
	if err != nil {
		return Accepted{}, err
	}
	if err = ctx.Err(); err != nil {
		return Accepted{}, err
	}
	if len(rawKeys) != 1 || rawKeys[0] == "" {
		return Accepted{}, &InputError{"idempotency_key", "REQUIRED"}
	}
	key, err := uuid.Parse(rawKeys[0])
	if err != nil || key == uuid.Nil {
		return Accepted{}, &InputError{"idempotency_key", "INVALID_FORMAT"}
	}
	digest := sha256.Sum256(content)
	request := idem.Request{Scope: user, Endpoint: s.method, Key: key.String(), Hash: idem.HashUpload(digest, header.FileName)}
	release, err := s.barrier.Shared(ctx, user)
	if err != nil {
		return Accepted{}, err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil && rpcErr == nil {
			rpcErr = releaseErr
		}
	}()
	// The whole reserved episode, including reserve COMMIT, fits its 15-second lease.
	work, stop := context.WithTimeout(ctx, 14*time.Second)
	defer stop()
	var token idem.Reservation
	var cached idem.Result
	var id string
	err = s.tx.Run(work, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, true); err != nil {
			return err
		}
		var err error
		token, cached, err = s.keys.Reserve(txCtx, request)
		if err != nil || cached.Replayed {
			return err
		}
		id, err = s.newID()
		if err != nil {
			return err
		}
		parsed, parseErr := uuid.Parse(id)
		if parseErr != nil || parsed == uuid.Nil || parsed.String() != id {
			return errAccounting
		}
		return s.keys.BindResource(txCtx, token, idem.Resource{Type: "upload", ID: id})
	})
	if err != nil {
		return Accepted{}, err
	}
	if cached.Replayed {
		if cached.Err != nil {
			return Accepted{}, cached.Err
		}
		return decodeAccepted(cached)
	}
	objectKey := user + "/" + id
	committed, possibleObject := false, false
	defer func() {
		if committed {
			return
		}
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = s.tx.Run(cleanup, func(txCtx context.Context) error {
			// Release is conditional on this exact reservation; it never erases a newer owner.
			return s.keys.Release(txCtx, token)
		})
		if possibleObject {
			// A lost COMMIT reply is not a proof of rollback. Serialize a fresh
			// primary reference check before deleting; uncertainty leaves cleanup to the durable sweep.
			retired := s.tx.Run(cleanup, func(txCtx context.Context) error {
				if err := s.guard(txCtx, user, false); err != nil {
					return err
				}
				return s.repo.AbandonStaging(txCtx, user, id, s.now())
			})
			if retired == nil {
				_ = s.deleteUnreferenced(cleanup, user, objectKey)
			}
		}
	}()
	// A single deadline fits the published 15-second reservation. Network calls
	// observe the caller; the library's detached completion is used only for the final transaction.
	refusal := s.parser.Validate(bytes.NewReader(content))
	if refusal == nil {
		// The intent survives a lost PUT reply, pod crash and purge phase one.
		if err := s.tx.Run(work, func(txCtx context.Context) error {
			if err := s.guard(txCtx, user, true); err != nil {
				return err
			}
			if err := s.keys.BindResource(txCtx, token, idem.Resource{Type: "upload", ID: id}); err != nil {
				return err
			}
			return s.repo.InsertUpload(txCtx, Upload{ID: id, UserID: user, FileName: header.FileName, ObjectKey: objectKey, SizeBytes: int64(len(content)), Digest: digest, Status: "staging", UploadedAt: s.now()})
		}); err != nil {
			return Accepted{}, err
		}
		possibleObject = true
		if err := s.objects.Put(work, objectKey, bytes.NewReader(content), int64(len(content))); err != nil {
			return Accepted{}, err
		}
	}
	completion, finish := s.keys.WithCompletion(work)
	defer finish()
	deadline, _ := work.Deadline()
	completion, bound := context.WithDeadline(completion, deadline)
	defer bound()
	err = s.tx.Run(completion, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user, true); err != nil {
			return err
		}
		var err error
		cached, err = s.keys.DoReserved(txCtx, token, func(effectCtx context.Context) (idem.Response, error) {
			if refusal != nil {
				return idem.Response{}, refusal
			}
			out = Accepted{UploadID: id, FileName: header.FileName}
			payload, err := json.Marshal(out)
			if err != nil {
				return idem.Response{}, err
			}
			err = s.repo.PromoteUpload(effectCtx, user, id, s.now())
			return idem.Response{Body: payload, Resource: idem.Resource{Type: "upload", ID: id}}, err
		})
		return err
	})
	if err != nil {
		return Accepted{}, err
	}
	committed = true
	if cached.Err != nil {
		return Accepted{}, cached.Err
	}
	return decodeAccepted(cached)
}

func decodeAccepted(result idem.Result) (Accepted, error) {
	var out Accepted
	decoder := json.NewDecoder(bytes.NewReader(result.Response.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || out.UploadID == "" || result.Response.Resource != (idem.Resource{Type: "upload", ID: out.UploadID}) {
		return Accepted{}, errAccounting
	}
	if id, err := uuid.Parse(out.UploadID); err != nil || id == uuid.Nil || !utf8.ValidString(out.FileName) || strings.ContainsRune(out.FileName, 0) {
		return Accepted{}, errAccounting
	}
	return out, nil
}

func (s *Service) guard(ctx context.Context, user string, create bool) error {
	alive, err := s.deleted.Guard(ctx, user)
	if err != nil {
		return err
	}
	if !alive {
		return ErrDeleted
	}
	_, err = s.repo.LockUser(ctx, user, create)
	return err
}

func (s *Service) deleteUnreferenced(ctx context.Context, user, key string) error {
	referenced := true
	err := s.tx.Run(ctx, func(txCtx context.Context) error {
		alive, err := s.deleted.Guard(txCtx, user)
		if err != nil {
			return err
		}
		if !alive {
			referenced = false
			return nil
		}
		if _, err = s.repo.LockUser(txCtx, user, false); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		referenced, err = s.repo.Referenced(txCtx, user, key)
		return err
	})
	if err != nil || referenced {
		return err
	}
	return s.objects.Delete(ctx, key)
}
