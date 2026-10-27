package app_test

import (
	"context"
	"errors"
	"os"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletion "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settings "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// The calculator adds no RPC: source admission and deletion use the real public
// service API; the FIN-357 worker consumes the assembled internal calculation seam.
func TestAPICalculationUsesOnlyCompletedPublicImportsAndHonorsPurge(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	client := pb.NewStatementsServiceClient(env.Conn)
	for _, file := range []string{"00-2025-10-15_2025-10-31.ofx", "01-2025-10-15_2026-06-30.ofx", "02-2026-06-20_2026-08-07.ofx", "03-2026-08-20_2026-09-25.ofx", "04-edge-cases-2026-09.ofx"} {
		raw, err := os.ReadFile("../adapter/ofx/testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), file, string(raw))
		if err != nil {
			t.Fatal(err)
		}
		apiComplete(t, client, accepted.UploadId)
	}
	calculationChecks(t, env.App.Calculations)
	foreign, err := client.GetImportConfig(apitest.User(t.Context(), apiOther), &pb.GetImportConfigRequest{})
	if err != nil || foreign.Loaded != nil {
		t.Fatal("other user's financial facts leaked", err)
	}
	purge := deletion.NewUserDeletionServiceClient(env.Conn)
	if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletion.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if _, err = env.App.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09")); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("late calculation resurrected deleted references", err)
	}
}

func TestAPICalculationRulesAndSelectionsHaveIndependentNumbers(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "rules.ofx", calculationRuleOFX())
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	calculationRuleChecks(t, env.App, apiUser)
}
func TestAPICalculationApiaImportAndFullMonthTerminate(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationApiaNow})
	client := pb.NewStatementsServiceClient(env.Conn)
	_, err := settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Pacific/Apia", SettingsVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "apia.ofx", calculationApiaBody(t))
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	calculationApiaCheck(t, env.App)
}

func TestAPICalculationAndPurgeSerializeRealWriters(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationNow})
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "rules.ofx", calculationRuleOFX())
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	calculationPurgeRace(t, env.App, func(ctx context.Context) error {
		_, err := deletion.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(ctx, "user"), &deletion.PurgeUserDataRequest{UserId: apiUser})
		return err
	})
}

func TestAPICalculationMidnightGapPreservesSQLCalendarDate(t *testing.T) {
	env := apitest.StartWith(t, app.Options{Now: calculationMidnightNow})
	client := pb.NewStatementsServiceClient(env.Conn)
	_, err := settings.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settings.SetUserTimezoneRequest{UserId: apiUser, Timezone: "America/Havana", SettingsVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "midnight.ofx", calculationMidnightBody(t))
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	calculationMidnightCheck(t, env.App)
}
