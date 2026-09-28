// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"bytes"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxAddressBytes      = 253
	maxArtifactIDBytes   = 64
	maxKeyMaterialBytes  = 16 << 10
	maxPathBytes         = 512
	maxVersionBytes      = 128
)

var (
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	accountNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	artifactIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	versionPattern       = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)
)

func (account AccountIdentity) validate(platform Platform) error {
	if len(account.Name) > maxAccountNameBytes {
		return ErrTooLarge
	}
	if !accountNamePattern.MatchString(account.Name) || account.Name == "." || account.Name == ".." {
		return ErrInvalid
	}
	if err := validateCanonicalPath(platform, account.HomePath); err != nil {
		return err
	}
	if err := validateCanonicalPath(platform, account.AuthorizedKeysPath); err != nil {
		return err
	}
	separator := "/"
	if platform == PlatformWindows {
		separator = `\`
	}
	if !strings.HasPrefix(account.AuthorizedKeysPath, account.HomePath+separator) {
		return ErrInvalid
	}
	return nil
}

func (key PublicKeyIdentity) validate() error {
	switch key.Algorithm {
	case PublicKeyAlgorithmED25519, PublicKeyAlgorithmRSA, PublicKeyAlgorithmECDSA:
	default:
		return ErrInvalid
	}
	if len(key.Material) == 0 {
		return ErrInvalid
	}
	if len(key.Material) > maxKeyMaterialBytes {
		return ErrTooLarge
	}
	if !sha256Pattern.MatchString(key.FingerprintSHA256) {
		return ErrInvalid
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(key.Material)
	if err != nil || base64.StdEncoding.EncodeToString(blob) != key.Material {
		return ErrInvalid
	}
	if !validPublicKeyBlob(key.Algorithm, blob) {
		return ErrInvalid
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != key.FingerprintSHA256 {
		return ErrInvalid
	}
	return nil
}

func validPublicKeyBlob(algorithm PublicKeyAlgorithm, blob []byte) bool {
	offset := 0
	encodedAlgorithm, ok := readSSHString(blob, &offset)
	if !ok || string(encodedAlgorithm) != string(algorithm) {
		return false
	}
	switch algorithm {
	case PublicKeyAlgorithmED25519:
		key, valid := readSSHString(blob, &offset)
		if !valid || len(key) != 32 || offset != len(blob) {
			return false
		}
		canonical := appendSSHString(nil, []byte(algorithm))
		canonical = appendSSHString(canonical, key)
		return bytes.Equal(canonical, blob)
	case PublicKeyAlgorithmRSA:
		exponentBytes, exponentValid := readSSHString(blob, &offset)
		modulusBytes, modulusValid := readSSHString(blob, &offset)
		if !exponentValid || !modulusValid || offset != len(blob) {
			return false
		}
		exponent, exponentCanonical := parsePositiveMPInt(exponentBytes)
		modulus, modulusCanonical := parsePositiveMPInt(modulusBytes)
		if !exponentCanonical || !modulusCanonical || !exponent.IsInt64() {
			return false
		}
		exponentValue := exponent.Int64()
		if exponentValue < 3 || exponentValue > math.MaxInt32 || exponentValue%2 == 0 || modulus.Bit(0) == 0 {
			return false
		}
		canonical := appendSSHString(nil, []byte(algorithm))
		canonical = appendSSHString(canonical, marshalPositiveMPInt(exponent))
		canonical = appendSSHString(canonical, marshalPositiveMPInt(modulus))
		return bytes.Equal(canonical, blob)
	case PublicKeyAlgorithmECDSA:
		curve, curveValid := readSSHString(blob, &offset)
		point, pointValid := readSSHString(blob, &offset)
		if !curveValid || !pointValid || string(curve) != "nistp256" || offset != len(blob) {
			return false
		}
		x, y := elliptic.Unmarshal(elliptic.P256(), point)
		if x == nil || y == nil {
			return false
		}
		canonical := appendSSHString(nil, []byte(algorithm))
		canonical = appendSSHString(canonical, curve)
		canonical = appendSSHString(canonical, elliptic.Marshal(elliptic.P256(), x, y))
		return bytes.Equal(canonical, blob)
	default:
		return false
	}
}

func parsePositiveMPInt(encoded []byte) (*big.Int, bool) {
	if len(encoded) == 0 || encoded[0]&0x80 != 0 {
		return nil, false
	}
	if len(encoded) > 1 && encoded[0] == 0 && encoded[1]&0x80 == 0 {
		return nil, false
	}
	value := new(big.Int).SetBytes(encoded)
	return value, value.Sign() > 0
}

func marshalPositiveMPInt(value *big.Int) []byte {
	encoded := value.Bytes()
	if encoded[0]&0x80 == 0 {
		return encoded
	}
	return append([]byte{0}, encoded...)
}

func appendSSHString(destination, value []byte) []byte {
	start := len(destination)
	destination = append(destination, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(destination[start:start+4], uint32(len(value)))
	return append(destination, value...)
}

func readSSHString(blob []byte, offset *int) ([]byte, bool) {
	if *offset < 0 || len(blob)-*offset < 4 {
		return nil, false
	}
	size := int(binary.BigEndian.Uint32(blob[*offset : *offset+4]))
	*offset += 4
	if size < 0 || size > len(blob)-*offset {
		return nil, false
	}
	value := blob[*offset : *offset+size]
	*offset += size
	return value, true
}

func (endpoint SSHEndpoint) validate() error {
	if len(endpoint.Address) == 0 || endpoint.Port < 1 || endpoint.Port > 65535 {
		return ErrInvalid
	}
	if len(endpoint.Address) > maxAddressBytes {
		return ErrTooLarge
	}
	address, err := netip.ParseAddr(endpoint.Address)
	if err != nil ||
		address.Zone() != "" ||
		address.String() != endpoint.Address {
		return ErrInvalid
	}
	return nil
}

func (artifact ArtifactIdentity) validate(platform Platform) error {
	if len(artifact.ID) > maxArtifactIDBytes || len(artifact.Version) > maxVersionBytes {
		return ErrTooLarge
	}
	if !artifactIDPattern.MatchString(artifact.ID) || !versionPattern.MatchString(artifact.Version) || !sha256Pattern.MatchString(artifact.SHA256) {
		return ErrInvalid
	}
	return validateCanonicalPath(platform, artifact.Path)
}

func validateCanonicalPath(platform Platform, value string) error {
	if len(value) == 0 {
		return ErrInvalid
	}
	if len(value) > maxPathBytes {
		return ErrTooLarge
	}
	if !utf8.ValidString(value) {
		return ErrInvalid
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character)
	}) >= 0 {
		return ErrInvalid
	}
	if platform == PlatformWindows {
		return validateWindowsPath(value)
	}
	if platform != PlatformDarwin && platform != PlatformLinux {
		return ErrInvalid
	}
	if value == "/" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || path.Clean(value) != value {
		return ErrInvalid
	}
	return nil
}

func validateWindowsPath(value string) error {
	if len(value) < 4 || value[0] < 'A' || value[0] > 'Z' || value[1:3] != `:\` || strings.Contains(value, "/") {
		return ErrInvalid
	}
	for _, segment := range strings.Split(value[3:], `\`) {
		if segment == "" || segment == "." || segment == ".." || strings.HasSuffix(segment, " ") || strings.HasSuffix(segment, ".") {
			return ErrInvalid
		}
		if strings.ContainsAny(segment, `<>:"|?*`) || reservedWindowsName(segment) {
			return ErrInvalid
		}
	}
	return nil
}

func reservedWindowsName(segment string) bool {
	name := strings.ToUpper(segment)
	if before, _, found := strings.Cut(name, "."); found {
		name = before
	}
	switch name {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	default:
		return false
	}
}
