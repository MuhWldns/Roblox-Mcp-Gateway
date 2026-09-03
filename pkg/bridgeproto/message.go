package bridgeproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const Version = 1

type MessageType string

const (
	TypeHello        MessageType = "hello"
	TypeHeartbeat    MessageType = "heartbeat"
	TypeStatus       MessageType = "status"
	TypeRequest      MessageType = "request"
	TypeResponse     MessageType = "response"
	TypeNotification MessageType = "notification"
	TypeCancel       MessageType = "cancel"
	TypeError        MessageType = "error"
)

type Envelope struct {
	Version          int             `json:"version"`
	Type             MessageType     `json:"type"`
	GatewayRequestID string          `json:"gateway_request_id,omitempty"`
	DeviceID         string          `json:"device_id"`
	StudioID         string          `json:"studio_id,omitempty"`
	Deadline         time.Time       `json:"deadline,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

type envelopeJSON struct {
	Version          int             `json:"version"`
	Type             MessageType     `json:"type"`
	GatewayRequestID string          `json:"gateway_request_id,omitempty"`
	DeviceID         string          `json:"device_id"`
	StudioID         string          `json:"studio_id,omitempty"`
	Deadline         *time.Time      `json:"deadline,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

func (envelope Envelope) MarshalJSON() ([]byte, error) {
	var deadline *time.Time
	if !envelope.Deadline.IsZero() {
		deadline = &envelope.Deadline
	}
	return json.Marshal(envelopeJSON{
		Version:          envelope.Version,
		Type:             envelope.Type,
		GatewayRequestID: envelope.GatewayRequestID,
		DeviceID:         envelope.DeviceID,
		StudioID:         envelope.StudioID,
		Deadline:         deadline,
		Payload:          envelope.Payload,
	})
}

func (envelope *Envelope) UnmarshalJSON(data []byte) error {
	var decoded envelopeJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := requireEndOfJSON(decoder); err != nil {
		return err
	}

	*envelope = Envelope{
		Version:          decoded.Version,
		Type:             decoded.Type,
		GatewayRequestID: decoded.GatewayRequestID,
		DeviceID:         decoded.DeviceID,
		StudioID:         decoded.StudioID,
		Payload:          decoded.Payload,
	}
	if decoded.Deadline != nil {
		envelope.Deadline = *decoded.Deadline
	}
	return nil
}

type Limits struct {
	MaxPayloadBytes int
}

func Encode(envelope Envelope, limits Limits) ([]byte, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode bridge envelope: %w", err)
	}
	if len(encoded) > limits.MaxPayloadBytes {
		return nil, fmt.Errorf("bridge envelope exceeds %d-byte limit", limits.MaxPayloadBytes)
	}
	return encoded, nil
}

func Decode(data []byte, limits Limits) (Envelope, error) {
	var envelope Envelope

	if err := validateLimits(limits); err != nil {
		return envelope, err
	}
	if len(data) > limits.MaxPayloadBytes {
		return envelope, fmt.Errorf("bridge envelope exceeds %d-byte limit", limits.MaxPayloadBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode bridge envelope: %w", err)
	}
	if err := requireEndOfJSON(decoder); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func requireEndOfJSON(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode bridge envelope: multiple JSON values")
		}
		return fmt.Errorf("decode bridge envelope trailing data: %w", err)
	}
	return nil
}
