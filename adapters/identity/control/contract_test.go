package control

import (
	"strings"
	"testing"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestIdentityWorkerContractHasOnlyFixedPrivateStreamsAndStrictState(t *testing.T) {
	file := platformv1.File_proactive_platform_v1_identity_worker_proto
	services := file.Services()
	if services.Len() != 1 {
		t.Fatalf("identity worker services = %d, want 1", services.Len())
	}
	service := services.Get(0)
	if string(service.Name()) != "IdentityWorkerControlService" || service.Methods().Len() != 2 {
		t.Fatalf("identity worker service = %s with %d methods", service.Name(), service.Methods().Len())
	}
	for index, name := range []string{"WatchVision", "WatchAudio"} {
		method := service.Methods().Get(index)
		if string(method.Name()) != name || method.IsStreamingClient() || !method.IsStreamingServer() {
			t.Fatalf("method %d = %s client_stream=%t server_stream=%t", index, method.Name(), method.IsStreamingClient(), method.IsStreamingServer())
		}
	}

	for _, messageName := range []protoreflect.Name{"WatchVisionResponse", "WatchAudioResponse"} {
		message := file.Messages().ByName(messageName)
		if message == nil || message.Oneofs().Len() != 1 || string(message.Oneofs().Get(0).Name()) != "state" || message.Oneofs().Get(0).Fields().Len() != 2 {
			t.Fatalf("%s does not have strict idle/active state oneof", messageName)
		}
	}
	audioActive := file.Messages().ByName("AudioActiveWork")
	if audioActive == nil || audioActive.Oneofs().Len() != 1 || string(audioActive.Oneofs().Get(0).Name()) != "task" || audioActive.Oneofs().Get(0).Fields().Len() != 2 {
		t.Fatal("AudioActiveWork does not have strict identification/verification task oneof")
	}

	verification := file.Messages().ByName("AudioSpeakerVerificationWork")
	for index := 0; index < verification.Fields().Len(); index++ {
		name := strings.ToLower(string(verification.Fields().Get(index).Name()))
		if strings.Contains(name, "profile") || strings.Contains(name, "template_ref") {
			t.Fatalf("verification contract leaked forbidden identity reference field %q", name)
		}
	}

	messages := file.Messages()
	for index := 0; index < messages.Len(); index++ {
		assertNoUntypedAny(t, messages.Get(index))
	}
}

func assertNoUntypedAny(t *testing.T, message protoreflect.MessageDescriptor) {
	t.Helper()
	for index := 0; index < message.Fields().Len(); index++ {
		field := message.Fields().Get(index)
		if field.Message() != nil && field.Message().FullName() == "google.protobuf.Any" {
			t.Fatalf("%s.%s uses forbidden google.protobuf.Any", message.FullName(), field.Name())
		}
	}
	for index := 0; index < message.Messages().Len(); index++ {
		assertNoUntypedAny(t, message.Messages().Get(index))
	}
}
