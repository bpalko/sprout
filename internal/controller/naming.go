/*
Copyright 2026 bpalko.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	identifierPrefix = "sprout_"
	// maxBaseLen leaves plenty of headroom under Postgres's 63-byte
	// identifier limit once the prefix, hash suffix, and role suffix are
	// added: 7 (prefix) + 30 (base) + 1 + 8 (hash) + 5 (role suffix) = 51.
	maxBaseLen = 30
)

// deriveDatabaseName returns a deterministic, valid identifier for the
// logical database backing a Sprout, derived from its namespace and name.
// It never depends on status, so it's stable across reconciles even if
// status is lost or the controller restarts.
func deriveDatabaseName(namespace, name string) string {
	base := sanitizeIdentifierPart(namespace + "_" + name)
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
	}
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	suffix := hex.EncodeToString(sum[:])[:8]
	return fmt.Sprintf("%s%s_%s", identifierPrefix, base, suffix)
}

// deriveRoleName returns the login role name owning databaseName.
func deriveRoleName(databaseName string) string {
	return databaseName + "_role"
}

func sanitizeIdentifierPart(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
