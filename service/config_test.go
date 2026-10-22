package service_test

import (
	"net"
	"strings"
	"testing"

	"github.com/xaroth/speki/service"
)

func TestNew(t *testing.T) {
	t.Parallel()
	base := validConfig(t)
	with := func(change func(*service.Config)) service.Config {
		c := base
		change(&c)
		return c
	}
	testCases := []struct {
		name string
		cfg  service.Config
		want []string // in the error; none for a config New takes
	}{
		{name: "key file", cfg: base},
		{name: "single key", cfg: with(func(c *service.Config) { c.Auth = service.Auth{Key: testSecret()} })},
		{
			name: "every problem at once",
			cfg:  service.Config{Listen: ":1", MetricsListen: ":1", Telemetry: service.Telemetry{LogLevel: "loud"}},
			want: []string{"metrics_listen", "log_level", "app_id", "private_key", "client_id", "public_url", "auth.keys"},
		},
		{
			name: "bad keys reported together",
			cfg: with(func(c *service.Config) {
				c.GitHub.PrivateKey, c.Auth.Keys, c.Telemetry.LogLevel = "not a key", "keys: []", "loud"
			}),
			want: []string{"github.private_key", "auth.keys", "log_level"},
		},
		{
			name: "same address by default",
			cfg:  with(func(c *service.Config) { c.Listen = ":9090" }),
			want: []string{"metrics_listen"},
		},
		{
			name: "query memory over 3/4 of the limit",
			cfg:  with(func(c *service.Config) { c.SQLite.MemoryLimit, c.SQLite.QueryMemory = 512, 400 }),
			want: []string{"sqlite.query_memory_mib"},
		},
		{
			name: "tiny memory limit",
			cfg:  with(func(c *service.Config) { c.SQLite.MemoryLimit = 64 }),
			want: []string{"sqlite.memory_limit_mib"},
		},
		{
			name: "plain http public URL",
			cfg:  with(func(c *service.Config) { c.PublicURL = "http://speki.example.com" }),
			want: []string{"https"},
		},
		{
			name: "bad single key",
			cfg:  with(func(c *service.Config) { c.Auth.Keys, c.Auth.Key = "", "c2hvcnQ=" }),
			want: []string{"auth.key:", "at least 32"},
		},
		{
			name: "single key and key file",
			cfg:  with(func(c *service.Config) { c.Auth.Key = testSecret() }),
			want: []string{"auth.key and auth.keys are both set"},
		},
		{
			name: "metadata host with a scheme",
			cfg: with(func(c *service.Config) {
				c.Auth.ClientMetadataHosts = []string{"https://claude.ai", "Claude.ai", "claude.ai."}
			}),
			want: []string{"client_metadata_hosts"},
		},
		{
			name: "account ID that isn't one",
			cfg:  with(func(c *service.Config) { c.GitHub.AllowedAccounts = []int64{0} }),
			want: []string{"allowed_accounts"},
		},
		{
			name: "bad instance",
			cfg: with(func(c *service.Config) {
				c.Instance = service.Instance{Name: "Work!", Title: "hidden\u202etext", Description: "tag\U000E0041"}
			}),
			want: []string{"instance.name:", "instance.title must", "instance.description must"},
		},
		{
			name: "negative interval",
			cfg:  with(func(c *service.Config) { c.Sync.DiscoveryInterval = -1 }),
			want: []string{"sync.discovery_interval"},
		},
		{
			name: "redis not answering",
			cfg:  with(func(c *service.Config) { c.Redis.URL = "redis://" + hangUp(t) + "?max_retries=-1" }),
			want: []string{"redis.url"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := create(testCase.cfg, discardLogger())
			if len(testCase.want) == 0 {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got no error, want one mentioning %q", testCase.want)
			}
			for _, w := range testCase.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("got error %q, want it to mention %q", err, w)
				}
			}
		})
	}
}

// hangUp listens for connections and closes each one straight away, so a client fails fast rather than
// retrying a refused dial.
func hangUp(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}
