package apitest

import (
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

// The whole service remains healthy and refuses unknown methods even after
// registering its business services.
func TestAPIServiceStartsAndServes(t *testing.T) {
	env := Start(t)
	resp, err := healthpb.NewHealthClient(env.Conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", resp, err)
	}
	err = env.Conn.Invoke(User(t.Context(), "u1"), "/finassist.nothing.v1.Nope/Get", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestAPIDevReflectionUsesConfiguredWholeAssembly(t *testing.T) {
	env := Start(t, func(cfg *config.Config) { cfg.GRPC.Reflection = true })
	stream, err := reflectionpb.NewServerReflectionClient(env.Conn).ServerReflectionInfo(Caller(t.Context(), "assistant"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.CloseSend() }()
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""}}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]bool{}
	for _, service := range response.GetListServicesResponse().GetService() {
		services[service.GetName()] = true
	}
	if len(services) != 10 || !services["finassist.statementdata.v1.AssistantToolsService"] || !services["finassist.statementdata.v1.OperationsService"] || !services["finassist.statementdata.v1.CategoriesService"] || !services["finassist.statementdata.v1.RegularPaymentsService"] || !services["grpc.health.v1.Health"] || !services["grpc.reflection.v1.ServerReflection"] || !services["grpc.reflection.v1alpha.ServerReflection"] {
		t.Fatalf("unexpected operational services: %v", services)
	}
	_, err = healthpb.NewHealthClient(env.Conn).Check(Caller(t.Context(), "assistant"), &healthpb.HealthCheckRequest{Service: "not-registered"})
	if status.Code(err) != codes.NotFound || Reason(err) != "" {
		t.Fatalf("unknown health service must return a bare status: %v", err)
	}
}

func TestUnitHelpers(t *testing.T) {
	md, _ := metadata.FromOutgoingContext(User(t.Context(), "u1"))
	if md.Get("authorization")[0] != "Bearer dev:u1" || md.Get("x-service-authorization")[0] != "Bearer dev:bank-bff" {
		t.Fatalf("user md: %v", md)
	}
	md, _ = metadata.FromOutgoingContext(Caller(t.Context(), "assistant"))
	if len(md.Get("authorization")) != 0 || md.Get("x-service-authorization")[0] != "Bearer dev:assistant" {
		t.Fatalf("caller md: %v", md)
	}
	if Reason(finerr.NotFound()) != "NOT_FOUND" || Reason(errors.New("x")) != "" || Reason(nil) != "" {
		t.Fatal("Reason")
	}
}
