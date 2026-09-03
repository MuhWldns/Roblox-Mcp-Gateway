package bridgeproto

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func validateLimits(limits Limits) error {
	if limits.MaxPayloadBytes <= 0 {
		return fmt.Errorf("max payload bytes must be positive")
	}
	return nil
}

func validateEnvelope(envelope Envelope) error {
	if envelope.Version != Version {
		return fmt.Errorf("unsupported bridge protocol version %d", envelope.Version)
	}
	if !envelope.Type.valid() {
		return fmt.Errorf("unsupported bridge message type %q", envelope.Type)
	}
	if envelope.DeviceID == "" {
		return fmt.Errorf("device_id is required for %q message", envelope.Type)
	}

	switch envelope.Type {
	case TypeHello, TypeStatus, TypeNotification:
		return requirePayload(envelope)
	case TypeRequest, TypeResponse, TypeError:
		if err := requireCorrelation(envelope); err != nil {
			return err
		}
		return requirePayload(envelope)
	case TypeCancel:
		return requireCorrelation(envelope)
	case TypeHeartbeat:
		return nil
	default:
		return fmt.Errorf("unsupported bridge message type %q", envelope.Type)
	}
}

func (messageType MessageType) valid() bool {
	switch messageType {
	case TypeHello, TypeHeartbeat, TypeStatus, TypeRequest, TypeResponse, TypeNotification, TypeCancel, TypeError:
		return true
	default:
		return false
	}
}

func requireCorrelation(envelope Envelope) error {
	if envelope.GatewayRequestID == "" {
		return fmt.Errorf("gateway_request_id is required for %q message", envelope.Type)
	}
	return nil
}

func requirePayload(envelope Envelope) error {
	if len(envelope.Payload) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Payload), []byte("null")) {
		return fmt.Errorf("payload is required for %q message", envelope.Type)
	}
	if !json.Valid(envelope.Payload) {
		return fmt.Errorf("payload for %q message must be valid JSON", envelope.Type)
	}
	return nil
}
