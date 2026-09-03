package mcpprocess

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
)

func validateRequest(frame json.RawMessage) error {
	object, err := decodeObject(frame)
	if err != nil {
		return err
	}
	if err := validateVersion(object); err != nil {
		return err
	}
	method, ok := object["method"]
	if !ok {
		return errors.New("JSON-RPC request is missing method")
	}
	var methodName string
	if err := json.Unmarshal(method, &methodName); err != nil || methodName == "" {
		return errors.New("JSON-RPC method must be a non-empty string")
	}
	if id, ok := object["id"]; ok {
		if err := validateID(id); err != nil {
			return err
		}
	}
	if params, ok := object["params"]; ok && !isObjectOrArray(params) {
		return errors.New("JSON-RPC params must be an object or array")
	}
	if _, ok := object["result"]; ok {
		return errors.New("JSON-RPC request must not contain result")
	}
	if _, ok := object["error"]; ok {
		return errors.New("JSON-RPC request must not contain error")
	}
	return nil
}

func validateResponse(frame, expectedID json.RawMessage) error {
	if err := validateID(expectedID); err != nil {
		return fmt.Errorf("expected correlation ID: %w", err)
	}
	object, err := decodeObject(frame)
	if err != nil {
		return err
	}
	if err := validateVersion(object); err != nil {
		return err
	}
	if _, ok := object["method"]; ok {
		return errors.New("JSON-RPC response must not contain method")
	}
	id, ok := object["id"]
	if !ok {
		return errors.New("JSON-RPC response is missing id")
	}
	if err := validateID(id); err != nil {
		return err
	}
	if !sameID(id, expectedID) {
		return fmt.Errorf("JSON-RPC response id %s does not match expected id %s", id, expectedID)
	}

	result, hasResult := object["result"]
	responseError, hasError := object["error"]
	if hasResult == hasError {
		if hasResult {
			return errors.New("JSON-RPC response must contain exactly one of result or error")
		}
		return errors.New("JSON-RPC response must contain result or error")
	}
	_ = result
	if hasError {
		if err := validateErrorObject(responseError); err != nil {
			return err
		}
	}
	return nil
}

func decodeObject(frame json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC JSON: %w", err)
	}
	if first := bytes.TrimSpace(raw); len(first) == 0 || first[0] != '{' {
		return nil, errors.New("JSON-RPC frame must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC JSON: %w", err)
	}
	if object == nil {
		return nil, errors.New("JSON-RPC frame must be an object")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("invalid JSON-RPC JSON: trailing value")
		}
		return nil, fmt.Errorf("invalid JSON-RPC JSON: %w", err)
	}
	return object, nil
}

func validateVersion(object map[string]json.RawMessage) error {
	version, ok := object["jsonrpc"]
	if !ok || string(version) != `"2.0"` {
		return errors.New(`JSON-RPC version must be "2.0"`)
	}
	return nil
}

func validateID(id json.RawMessage) error {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return errors.New("JSON-RPC id must be a string or integer")
	}
	var text string
	if json.Unmarshal(id, &text) == nil {
		return nil
	}
	var integer big.Int
	if _, ok := integer.SetString(string(id), 10); ok {
		return nil
	}
	return errors.New("JSON-RPC id must be a string or integer")
}

func sameID(left, right json.RawMessage) bool {
	var leftText, rightText string
	leftIsText := json.Unmarshal(left, &leftText) == nil
	rightIsText := json.Unmarshal(right, &rightText) == nil
	if leftIsText || rightIsText {
		return leftIsText && rightIsText && leftText == rightText
	}
	var leftInteger, rightInteger big.Int
	_, leftOK := leftInteger.SetString(string(left), 10)
	_, rightOK := rightInteger.SetString(string(right), 10)
	return leftOK && rightOK && leftInteger.Cmp(&rightInteger) == 0
}

func isObjectOrArray(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && (value[0] == '{' || value[0] == '[')
}

func validateErrorObject(value json.RawMessage) error {
	object, err := decodeObject(value)
	if err != nil {
		return errors.New("JSON-RPC error must be an object")
	}
	code, ok := object["code"]
	if !ok {
		return errors.New("JSON-RPC error is missing code")
	}
	var integer big.Int
	if _, ok := integer.SetString(string(code), 10); !ok {
		return errors.New("JSON-RPC error code must be an integer")
	}
	message, ok := object["message"]
	if !ok {
		return errors.New("JSON-RPC error is missing message")
	}
	var text string
	if err := json.Unmarshal(message, &text); err != nil {
		return errors.New("JSON-RPC error message must be a string")
	}
	return nil
}
