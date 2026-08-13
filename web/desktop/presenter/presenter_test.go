package presenter

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
)

func TestBuildMapsDesktopStateToStableChineseView(t *testing.T) {
	state := &platformv1.DesktopState{
		Revision: 9,
		Avatar: &platformv1.AvatarSnapshot{
			Mode: platformv1.AvatarMode_AVATAR_MODE_SPEAKING, Text: "你回来啦。", Speaking: true,
		},
		Permissions: []*platformv1.PermissionGrant{
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE, Enabled: true},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE, Enabled: true},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION},
			{Permission: platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION},
		},
		Providers: []*platformv1.ProviderRuntime{
			{ProviderId: "desktop-vad", State: platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DEGRADED, Reason: platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_DEVICE_UNAVAILABLE},
			{ProviderId: "desktop-presence", State: platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_RUNNING, Reason: platformv1.ProviderRuntimeReason_PROVIDER_RUNTIME_REASON_NONE},
		},
	}

	view, err := Build(state)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if view.Revision != 9 || view.Avatar.Mode != "正在说话" || view.Avatar.CSSClass != "speaking" || view.Avatar.Text != "你回来啦。" || !view.Avatar.Speaking {
		t.Fatalf("Build().Avatar = %#v revision=%d", view.Avatar, view.Revision)
	}
	wantPermissionIDs := []string{
		"camera-capture", "microphone-capture", "face-detection", "face-identification",
		"face-liveness", "speaker-identification", "speaker-verification", "speech-transcription",
	}
	gotPermissionIDs := make([]string, len(view.Permissions))
	for index, permission := range view.Permissions {
		gotPermissionIDs[index] = permission.ID
	}
	if !reflect.DeepEqual(gotPermissionIDs, wantPermissionIDs) {
		t.Fatalf("permission order = %#v, want %#v", gotPermissionIDs, wantPermissionIDs)
	}
	if !view.Permissions[0].Enabled || !view.Permissions[1].Enabled || view.Permissions[2].Enabled {
		t.Fatalf("permissions = %#v, want only camera and microphone enabled", view.Permissions)
	}
	if !view.CameraEnabled || !view.MicrophoneEnabled || view.BiometricsEnabled {
		t.Fatalf("capture indicators = camera:%t microphone:%t biometric:%t", view.CameraEnabled, view.MicrophoneEnabled, view.BiometricsEnabled)
	}
	if got := []string{view.Providers[0].ID, view.Providers[1].ID}; !reflect.DeepEqual(got, []string{"desktop-presence", "desktop-vad"}) {
		t.Fatalf("provider order = %#v", got)
	}
	if view.Providers[1].State != "已降级" || view.Providers[1].Reason != "设备不可用" {
		t.Fatalf("degraded provider = %#v", view.Providers[1])
	}
}

func TestBuildRejectsMalformedOrUnknownState(t *testing.T) {
	valid := completeState()
	for _, test := range []struct {
		name   string
		mutate func(*platformv1.DesktopState)
	}{
		{name: "nil state", mutate: func(state *platformv1.DesktopState) {}},
		{name: "zero revision", mutate: func(state *platformv1.DesktopState) { state.Revision = 0 }},
		{name: "missing avatar", mutate: func(state *platformv1.DesktopState) { state.Avatar = nil }},
		{name: "unknown avatar", mutate: func(state *platformv1.DesktopState) { state.Avatar.Mode = 999 }},
		{name: "incomplete permissions", mutate: func(state *platformv1.DesktopState) { state.Permissions = state.Permissions[:7] }},
		{name: "duplicate permission", mutate: func(state *platformv1.DesktopState) {
			state.Permissions[7].Permission = state.Permissions[0].Permission
		}},
		{name: "unknown permission", mutate: func(state *platformv1.DesktopState) { state.Permissions[0].Permission = 999 }},
		{name: "empty provider id", mutate: func(state *platformv1.DesktopState) {
			state.Providers = []*platformv1.ProviderRuntime{{State: platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_RUNNING}}
		}},
		{name: "unknown provider state", mutate: func(state *platformv1.DesktopState) {
			state.Providers = []*platformv1.ProviderRuntime{{ProviderId: "provider", State: 999}}
		}},
		{name: "unknown provider reason", mutate: func(state *platformv1.DesktopState) {
			state.Providers = []*platformv1.ProviderRuntime{{ProviderId: "provider", State: platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DEGRADED, Reason: 999}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state *platformv1.DesktopState
			if test.name != "nil state" {
				state = cloneState(valid)
			}
			test.mutate(state)
			if _, err := Build(state); err == nil {
				t.Fatal("Build() error = nil, want fail closed")
			}
		})
	}
}

func completeState() *platformv1.DesktopState {
	permissions := []platformv1.DesktopPermission{
		platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION,
	}
	state := &platformv1.DesktopState{
		Revision: 1,
		Avatar:   &platformv1.AvatarSnapshot{Mode: platformv1.AvatarMode_AVATAR_MODE_IDLE},
	}
	for _, permission := range permissions {
		state.Permissions = append(state.Permissions, &platformv1.PermissionGrant{Permission: permission})
	}
	return state
}

func cloneState(input *platformv1.DesktopState) *platformv1.DesktopState {
	return proto.Clone(input).(*platformv1.DesktopState)
}
