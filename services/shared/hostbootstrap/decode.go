// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DecodeRequest strictly decodes and validates one request document.
func DecodeRequest(raw []byte) (Request, error) {
	return decodeStrict(raw, Request.Validate)
}

// DecodePlan strictly decodes and validates one deterministic plan.
func DecodePlan(raw []byte) (Plan, error) {
	return decodeStrict(raw, Plan.Validate)
}

// DecodeReceipt strictly decodes and validates one completion receipt.
func DecodeReceipt(raw []byte) (Receipt, error) {
	return decodeStrict(raw, Receipt.Validate)
}

// DecodeStatus strictly decodes and validates one operation status.
func DecodeStatus(raw []byte) (Status, error) {
	return decodeStrict(raw, Status.Validate)
}

func decodeStrict[T any](raw []byte, validate func(T) error) (T, error) {
	var value T
	if len(raw) == 0 {
		return value, ErrInvalid
	}
	if len(raw) > maxPayloadBytes {
		return value, ErrTooLarge
	}
	if !utf8.Valid(raw) {
		return value, ErrInvalid
	}
	if !validJSONUnicodeEscapes(raw) {
		return value, ErrInvalid
	}
	if err := inspectJSON(raw, reflect.TypeFor[T]()); err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, ErrInvalid
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return value, ErrInvalid
	}
	if err := validate(value); err != nil {
		return value, err
	}
	return value, nil
}

func inspectJSON(raw []byte, expected reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder, expected); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder, expected reflect.Type) error {
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalid
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if token == nil {
			if expected.Kind() == reflect.Pointer {
				return nil
			}
			return ErrInvalid
		}
		if !scalarTokenMatches(expected, token) {
			return ErrInvalid
		}
		if text, isString := token.(string); isString && containsPrivateKey(text) {
			return ErrProhibited
		}
		return nil
	}
	switch delimiter {
	case '{':
		expected = indirectType(expected)
		if expected.Kind() != reflect.Struct {
			return ErrInvalid
		}
		fields := canonicalJSONFields(expected)
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return ErrInvalid
			}
			key, isString := keyToken.(string)
			if !isString {
				return ErrInvalid
			}
			if prohibitedField(key) {
				return ErrProhibited
			}
			fieldType, canonical := fields[key]
			if !canonical {
				return ErrInvalid
			}
			folded := strings.ToLower(key)
			if _, duplicate := seen[folded]; duplicate {
				return ErrInvalid
			}
			seen[folded] = struct{}{}
			if err := inspectJSONValue(decoder, fieldType); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil || closing != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		expected = indirectType(expected)
		if expected.Kind() != reflect.Array && expected.Kind() != reflect.Slice {
			return ErrInvalid
		}
		for decoder.More() {
			if err := inspectJSONValue(decoder, expected.Elem()); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil || closing != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func scalarTokenMatches(expected reflect.Type, token any) bool {
	expected = indirectType(expected)
	switch expected.Kind() {
	case reflect.String:
		_, matches := token.(string)
		return matches
	case reflect.Bool:
		_, matches := token.(bool)
		return matches
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, matches := token.(json.Number)
		return matches
	case reflect.Float32, reflect.Float64:
		_, matches := token.(json.Number)
		return matches
	default:
		return false
	}
}

func indirectType(value reflect.Type) reflect.Type {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value
}

func canonicalJSONFields(value reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, value.NumField())
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

func validJSONUnicodeEscapes(raw []byte) bool {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString {
				continue
			}
			index++
			if index >= len(raw) {
				return false
			}
			if raw[index] != 'u' {
				continue
			}
			codePoint, valid := decodeHexQuad(raw, index+1)
			if !valid {
				return false
			}
			index += 4
			switch {
			case codePoint >= 0xd800 && codePoint <= 0xdbff:
				if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
					return false
				}
				low, lowValid := decodeHexQuad(raw, index+3)
				if !lowValid || low < 0xdc00 || low > 0xdfff {
					return false
				}
				index += 6
			case codePoint >= 0xdc00 && codePoint <= 0xdfff:
				return false
			}
		}
	}
	return true
}

func decodeHexQuad(raw []byte, start int) (uint16, bool) {
	if start < 0 || len(raw)-start < 4 {
		return 0, false
	}
	var value uint16
	for _, encoded := range raw[start : start+4] {
		digit, valid := hexDigit(encoded)
		if !valid {
			return 0, false
		}
		value = value<<4 | uint16(digit)
	}
	return value, true
}

func hexDigit(encoded byte) (byte, bool) {
	switch {
	case encoded >= '0' && encoded <= '9':
		return encoded - '0', true
	case encoded >= 'a' && encoded <= 'f':
		return encoded - 'a' + 10, true
	case encoded >= 'A' && encoded <= 'F':
		return encoded - 'A' + 10, true
	default:
		return 0, false
	}
}

func prohibitedField(field string) bool {
	normalized := strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			return unicode.ToLower(character)
		}
		return -1
	}, field)
	switch normalized {
	case "password", "passwords", "passphrase", "passphrases",
		"privatekey", "privatekeys", "token", "tokens", "apitoken",
		"accesstoken", "refreshtoken", "bearertoken", "secret", "secrets",
		"clientsecret", "authorization", "credential", "credentials",
		"command", "commands", "shell", "argv", "args", "script", "scripts":
		return true
	default:
		return false
	}
}

func containsPrivateKey(value string) bool {
	upper := strings.ToUpper(value)
	return strings.Contains(upper, "PRIVATE KEY") || strings.Contains(upper, "OPENSSH PRIVATE")
}
