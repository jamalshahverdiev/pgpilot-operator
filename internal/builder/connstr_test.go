package builder

import (
	"net/url"
	"strings"
	"testing"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// For the names almost everyone uses, the connection string has to stay
// byte-for-byte what it was: it is part of sources.yaml, whose hash rolls the
// collector pod.
func TestBuildConnStr_PlainNamesAreUnchanged(t *testing.T) {
	tests := []struct {
		name string
		db   pgpilotv1.DatabaseRef
		want string
	}{
		{
			name: "DNS name with explicit port and sslmode",
			db:   pgpilotv1.DatabaseRef{Host: "pg.example.com", Port: 5432, Database: "mydb", SSLMode: pgpilotv1.SSLModeRequire},
			want: "postgresql://pg.example.com:5432/mydb?sslmode=require",
		},
		{
			name: "defaults for port and sslmode",
			db:   pgpilotv1.DatabaseRef{Host: "postgres", Database: "app"},
			want: "postgresql://postgres:5432/app?sslmode=disable",
		},
		{
			name: "custom port",
			db:   pgpilotv1.DatabaseRef{Host: "10.0.0.5", Port: 6432, Database: "app_db-1", SSLMode: pgpilotv1.SSLModeVerifyFull},
			want: "postgresql://10.0.0.5:6432/app_db-1?sslmode=verify-full",
		},
		{
			name: "underscores and dots",
			db:   pgpilotv1.DatabaseRef{Host: "my_db-1.internal.", Port: 5432, Database: "orders.v2", SSLMode: pgpilotv1.SSLModePrefer},
			want: "postgresql://my_db-1.internal.:5432/orders.v2?sslmode=prefer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildConnStr(tt.db); got != tt.want {
				t.Errorf("buildConnStr = %q, want %q", got, tt.want)
			}
		})
	}
}

// A bare IPv6 address used to be glued to the port ("fd00::10:5432"), which
// is a different address; the full form did not parse at all.
func TestBuildConnStr_IPv6(t *testing.T) {
	tests := []struct {
		host     string
		wantHost string
	}{
		{host: "fd00::10", wantHost: "fd00::10"},
		{host: "[fd00::10]", wantHost: "fd00::10"},
		{host: "2001:db8:0:0:0:0:0:1", wantHost: "2001:db8:0:0:0:0:0:1"},
		{host: "::1", wantHost: "::1"},
		{host: "::ffff:10.0.0.5", wantHost: "::ffff:10.0.0.5"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := buildConnStr(pgpilotv1.DatabaseRef{Host: tt.host, Port: 6432, Database: "app"})

			want := "postgresql://[" + tt.wantHost + "]:6432/app?sslmode=disable"
			if got != want {
				t.Fatalf("buildConnStr = %q, want %q", got, want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
			if u.Hostname() != tt.wantHost || u.Port() != "6432" {
				t.Errorf("parsed host %q port %q, want %q and 6432", u.Hostname(), u.Port(), tt.wantHost)
			}
		})
	}
}

// PostgreSQL accepts almost anything in a database name. Unescaped, '#' or
// '?' ended the path early: the collector connected to a truncated name and
// the sslmode behind it was lost.
func TestBuildConnStr_DatabaseNameSurvivesTheRoundTrip(t *testing.T) {
	names := []string{
		"app#1", "app?x=1", "100%", "my db", "a/b", "a&b=c", "ünïcode", "with+plus", "semi;colon", "at@sign",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			got := buildConnStr(pgpilotv1.DatabaseRef{
				Host: "pg.example.com", Port: 5432, Database: name, SSLMode: pgpilotv1.SSLModeVerifyFull,
			})

			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("%q does not parse: %v", got, err)
			}
			// This is how pgx, which pgwatch uses, reads the URL.
			if db := strings.TrimLeft(u.Path, "/"); db != name {
				t.Errorf("database read back as %q, want %q (URL %q)", db, name, got)
			}
			if u.Hostname() != "pg.example.com" || u.Port() != "5432" {
				t.Errorf("host/port read back as %q/%q (URL %q)", u.Hostname(), u.Port(), got)
			}
			if mode := u.Query().Get("sslmode"); mode != "verify-full" {
				t.Errorf("sslmode read back as %q, want verify-full (URL %q)", mode, got)
			}
			if len(u.Query()) != 1 || u.Fragment != "" || u.User != nil {
				t.Errorf("unexpected URL parts in %q: query %v, fragment %q, user %v", got, u.Query(), u.Fragment, u.User)
			}
		})
	}
}

func TestBuildConnStr_NeverCarriesCredentials(t *testing.T) {
	got := buildConnStr(pgpilotv1.DatabaseRef{
		Host: "pg.example.com", Database: "app", Username: "pgwatch", Password: "s3cret",
	})
	if strings.Contains(got, "pgwatch") || strings.Contains(got, "s3cret") || strings.Contains(got, "@") {
		t.Errorf("connection string carries credentials: %q", got)
	}
}
