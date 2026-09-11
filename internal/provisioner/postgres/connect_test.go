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

package postgres

import (
	"strings"
	"testing"

	"github.com/bpalko/sprout/internal/provisioner"
)

func TestConnStringDefaults(t *testing.T) {
	got, err := connString(provisioner.ConnectionConfig{
		Host: "h", Port: 5432,
	}, "u", "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "dbname="+provisioner.DefaultAdminDatabase) {
		t.Fatalf("missing default dbname: %s", got)
	}
	if !strings.Contains(got, "sslmode="+provisioner.DefaultSSLMode) {
		t.Fatalf("missing default sslmode: %s", got)
	}
}

func TestConnStringOverrides(t *testing.T) {
	got, err := connString(provisioner.ConnectionConfig{
		Host: "h", Port: 5432, SSLMode: "require",
	}, "admin", "pw", "rdsadmin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "dbname=rdsadmin") {
		t.Fatalf("missing dbname: %s", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Fatalf("missing sslmode: %s", got)
	}
}

func TestConnStringRejectsUnknownSSLMode(t *testing.T) {
	_, err := connString(provisioner.ConnectionConfig{SSLMode: "prefer dbname=stolen"}, "u", "p", "postgres")
	if err == nil {
		t.Fatal("expected error for injected sslmode")
	}
}

func TestConnStringRejectsInjectedDatabase(t *testing.T) {
	_, err := connString(provisioner.ConnectionConfig{}, "u", "p", "postgres sslmode=disable")
	if err == nil {
		t.Fatal("expected error for injected dbname")
	}
}
