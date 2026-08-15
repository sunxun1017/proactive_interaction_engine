package biometricvault

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	// FaceInferenceProfileID identifies the complete pinned SFace extraction
	// profile used by both enrollment and candidate evidence.
	FaceInferenceProfileID = "opencv-sface-2021dec-profile-v1"
	// SpeakerInferenceProfileID identifies the complete pinned ERes2Net
	// extraction profile shared by identification and verification.
	SpeakerInferenceProfileID = "modelscope-eres2net-v1.0.5-profile-v1"

	FaceModelSHA256    = "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79"
	SpeakerModelSHA256 = "ad78a02cab9dc23385c3fe235d1c3a370c0bd89ef192d0250546b370b0b8f227"

	faceTemplateHeaderBytes    = 50
	faceTemplateDimension      = 128
	faceTemplateBytes          = faceTemplateHeaderBytes + faceTemplateDimension*4
	speakerTemplateHeaderBytes = 52
	speakerTemplateDimension   = 192
	speakerTemplateBytes       = speakerTemplateHeaderBytes + speakerTemplateDimension*4
	templateUnitTolerance      = 1e-4
	templateCodecOp            = "validate biometric template codec"
)

var (
	faceModelDigest    = decodePinnedDigest(FaceModelSHA256)
	speakerModelDigest = decodePinnedDigest(SpeakerModelSHA256)
)

func validateTemplatePayload(registration biometric.Registration, payload []byte) error {
	switch registration.Capability {
	case readiness.FaceIdentification:
		if registration.ModelVersion != FaceInferenceProfileID {
			return invalidTemplate("face inference profile is unsupported")
		}
		return validateFaceTemplate(payload)
	case readiness.SpeakerIdentification, readiness.SpeakerVerification:
		if registration.ModelVersion != SpeakerInferenceProfileID {
			return invalidTemplate("speaker inference profile is unsupported")
		}
		return validateSpeakerTemplate(payload)
	default:
		return invalidTemplate("capability does not store an identity template")
	}
}

func validateFaceTemplate(payload []byte) error {
	if len(payload) != faceTemplateBytes {
		return invalidTemplate("face template size is invalid")
	}
	if string(payload[:8]) != "PIEFACE1" ||
		binary.LittleEndian.Uint16(payload[8:10]) != 1 ||
		binary.LittleEndian.Uint16(payload[10:12]) != 1 ||
		binary.LittleEndian.Uint16(payload[12:14]) != faceTemplateDimension ||
		binary.LittleEndian.Uint16(payload[14:16]) == 0 ||
		binary.LittleEndian.Uint16(payload[16:18]) != 0x0101 ||
		!bytes.Equal(payload[18:50], faceModelDigest) {
		return invalidTemplate("face template header is invalid")
	}
	return validateUnitVector(payload[faceTemplateHeaderBytes:], binary.LittleEndian, faceTemplateDimension, "face")
}

func validateSpeakerTemplate(payload []byte) error {
	if len(payload) != speakerTemplateBytes {
		return invalidTemplate("speaker template size is invalid")
	}
	count := binary.BigEndian.Uint16(payload[16:18])
	if string(payload[:8]) != "SPKRTPL1" ||
		binary.BigEndian.Uint16(payload[8:10]) != 1 ||
		binary.BigEndian.Uint16(payload[10:12]) != speakerTemplateDimension ||
		binary.BigEndian.Uint32(payload[12:16]) != 16_000 ||
		count == 0 || count > 16 ||
		binary.BigEndian.Uint16(payload[18:20]) != 0 ||
		!bytes.Equal(payload[20:52], speakerModelDigest) {
		return invalidTemplate("speaker template header is invalid")
	}
	return validateUnitVector(payload[speakerTemplateHeaderBytes:], binary.BigEndian, speakerTemplateDimension, "speaker")
}

func validateUnitVector(encoded []byte, order binary.ByteOrder, dimension int, kind string) error {
	sum := 0.0
	for index := 0; index < dimension; index++ {
		value := float64(math.Float32frombits(order.Uint32(encoded[index*4 : index*4+4])))
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return invalidTemplate(kind + " template contains a non-finite value")
		}
		sum += value * value
	}
	norm := math.Sqrt(sum)
	if math.IsNaN(norm) || math.IsInf(norm, 0) || math.Abs(norm-1) > templateUnitTolerance {
		return invalidTemplate(kind + " template is not L2 normalized")
	}
	return nil
}

func invalidTemplate(message string) error {
	return fault.New(fault.InvalidInput, templateCodecOp, errors.New(message))
}

func decodePinnedDigest(value string) []byte {
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != 32 {
		panic("invalid pinned biometric model digest")
	}
	return digest
}
