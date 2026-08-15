package biometricvault

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestFixedTemplateCodecsMatchPinnedPythonFormats(t *testing.T) {
	for _, test := range []struct {
		name         string
		registration biometric.Registration
		payload      []byte
		pythonSHA256 string
	}{
		{
			name: "face identification",
			registration: biometric.Registration{Capability: readiness.FaceIdentification,
				ModelVersion: FaceInferenceProfileID},
			payload:      validFaceTemplatePayload(),
			pythonSHA256: "89cf3095d70c9fab2448d4b94f851eb301d8507041b773a42f0a283e4cf18734",
		},
		{
			name: "speaker identification",
			registration: biometric.Registration{Capability: readiness.SpeakerIdentification,
				ModelVersion: SpeakerInferenceProfileID},
			payload:      validSpeakerTemplatePayload(),
			pythonSHA256: "6fdd0e20cb295fd5b16f3b014fafe10846da2633f2a86f63127943ecb51e093d",
		},
		{
			name: "speaker verification",
			registration: biometric.Registration{Capability: readiness.SpeakerVerification,
				ModelVersion: SpeakerInferenceProfileID},
			payload:      validSpeakerTemplatePayload(),
			pythonSHA256: "6fdd0e20cb295fd5b16f3b014fafe10846da2633f2a86f63127943ecb51e093d",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := sha256.Sum256(test.payload)
			if got := hex.EncodeToString(digest[:]); got != test.pythonSHA256 {
				t.Fatalf("fixture SHA-256 = %s, want Python codec output %s", got, test.pythonSHA256)
			}
			if err := validateTemplatePayload(test.registration, test.payload); err != nil {
				t.Fatalf("validateTemplatePayload() error = %v", err)
			}
		})
	}
}

func TestFixedTemplateCodecsRejectWrongCapabilityProfileAndPayload(t *testing.T) {
	face := validFaceTemplatePayload()
	speaker := validSpeakerTemplatePayload()
	faceNaN := append([]byte(nil), face...)
	binary.LittleEndian.PutUint32(faceNaN[faceTemplateHeaderBytes:], math.Float32bits(float32(math.NaN())))
	speakerWrongDigest := append([]byte(nil), speaker...)
	speakerWrongDigest[20] ^= 0xff

	tests := []struct {
		name         string
		registration biometric.Registration
		payload      []byte
	}{
		{name: "detection has no template", registration: biometric.Registration{Capability: readiness.FaceDetection, ModelVersion: FaceInferenceProfileID}, payload: face},
		{name: "face profile mismatch", registration: biometric.Registration{Capability: readiness.FaceIdentification, ModelVersion: SpeakerInferenceProfileID}, payload: face},
		{name: "speaker profile mismatch", registration: biometric.Registration{Capability: readiness.SpeakerIdentification, ModelVersion: FaceInferenceProfileID}, payload: speaker},
		{name: "face bytes for speaker", registration: biometric.Registration{Capability: readiness.SpeakerVerification, ModelVersion: SpeakerInferenceProfileID}, payload: face},
		{name: "raw media", registration: biometric.Registration{Capability: readiness.FaceIdentification, ModelVersion: FaceInferenceProfileID}, payload: []byte("\xff\xd8\xffraw-jpeg")},
		{name: "face nonfinite", registration: biometric.Registration{Capability: readiness.FaceIdentification, ModelVersion: FaceInferenceProfileID}, payload: faceNaN},
		{name: "speaker wrong digest", registration: biometric.Registration{Capability: readiness.SpeakerIdentification, ModelVersion: SpeakerInferenceProfileID}, payload: speakerWrongDigest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateTemplatePayload(test.registration, test.payload); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("validateTemplatePayload() error = %v, want InvalidInput", err)
			}
		})
	}
}

func validFaceTemplatePayload() []byte {
	return validFaceTemplatePayloadAt(0)
}

func validFaceTemplatePayloadAt(index int) []byte {
	payload := make([]byte, faceTemplateBytes)
	copy(payload, []byte("PIEFACE1"))
	binary.LittleEndian.PutUint16(payload[8:10], 1)
	binary.LittleEndian.PutUint16(payload[10:12], 1)
	binary.LittleEndian.PutUint16(payload[12:14], 128)
	binary.LittleEndian.PutUint16(payload[14:16], 1)
	binary.LittleEndian.PutUint16(payload[16:18], 0x0101)
	copy(payload[18:50], mustDigest(FaceModelSHA256))
	binary.LittleEndian.PutUint32(payload[faceTemplateHeaderBytes+index*4:], math.Float32bits(1))
	return payload
}

func validSpeakerTemplatePayload() []byte {
	payload := make([]byte, speakerTemplateBytes)
	copy(payload, []byte("SPKRTPL1"))
	binary.BigEndian.PutUint16(payload[8:10], 1)
	binary.BigEndian.PutUint16(payload[10:12], 192)
	binary.BigEndian.PutUint32(payload[12:16], 16_000)
	binary.BigEndian.PutUint16(payload[16:18], 1)
	binary.BigEndian.PutUint16(payload[18:20], 0)
	copy(payload[20:52], mustDigest(SpeakerModelSHA256))
	binary.BigEndian.PutUint32(payload[speakerTemplateHeaderBytes:], math.Float32bits(1))
	return payload
}

func mustDigest(value string) []byte {
	digest, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return digest
}
