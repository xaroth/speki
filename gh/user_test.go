package gh_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eve-online-tools/speki/gh"
)

func TestMe(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		token   string
		want    gh.User
		wantErr error
	}{
		{name: "user", token: writer, want: gh.User{ID: 7, Login: "octocat"}},
		{name: "token revoked", token: revoked, wantErr: gh.ErrUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB(), withUsers())
			got, err := fake.Me(t.Context(), testCase.token)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got %+v, want %+v", got, testCase.want)
			}
		})
	}
}

func TestActiveMember(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		token   string
		org     string
		want    bool
		wantErr error
	}{
		{name: "active", token: writer, org: "ours", want: true},
		{name: "invited", token: writer, org: "invited"},
		{name: "not a member", token: writer, org: "strangers"},
		{name: "token revoked", token: revoked, org: "ours", wantErr: gh.ErrUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB(), withUsers(),
				gh.ExpectOrgMembership(writer, "ours", "active"),
				gh.ExpectOrgMembership(writer, "invited", "pending"))
			got, err := fake.ActiveMember(t.Context(), testCase.token, testCase.org)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestTeamMember(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		token   string
		team    string
		want    bool
		wantErr error
	}{
		{name: "active", token: writer, team: "editors", want: true},
		{name: "invited", token: writer, team: "invited"},
		{name: "team not seen", token: writer, team: "secret"},
		{name: "token revoked", token: revoked, team: "editors", wantErr: gh.ErrUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, withKB(), withUsers(),
				gh.ExpectTeamMembership("ours", "editors", "octocat", "active"),
				gh.ExpectTeamMembership("ours", "invited", "octocat", "pending"))
			got, err := fake.TeamMember(t.Context(), testCase.token, "ours", testCase.team, "octocat")
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("got %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestUserRepositories(t *testing.T) {
	t.Parallel()
	repos := []gh.Repository{{ID: 101, FullName: "ours/a"}, {ID: 102, FullName: "ours/b"}, {ID: 103, FullName: "ours/c"}}
	testCases := []struct {
		name    string
		token   string
		want    []gh.RepoAccess
		wantErr error
	}{
		{
			name:  "every role, paged, across installations",
			token: writer,
			want: []gh.RepoAccess{
				{RepoID: 100, Role: gh.RoleWrite},
				{RepoID: 101, Role: gh.RoleRead},
				{RepoID: 102, Role: gh.RoleTriage},
				{RepoID: 103, Role: gh.RoleMaintain},
				{RepoID: 200, Role: gh.RoleAdmin},
			},
		},
		{name: "token revoked", token: revoked, wantErr: gh.ErrUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := gh.NewFake(t, gh.ExpectPageSize(2),
				gh.ExpectInstallation(gh.Installation{ID: 1}, append([]gh.Repository{kb}, repos...)...),
				gh.ExpectInstallation(gh.Installation{ID: 2}, gh.Repository{ID: 200, FullName: "theirs/kb"}),
				gh.ExpectInstallation(gh.Installation{ID: 3}, gh.Repository{ID: 300, FullName: "theirs/hidden"}),
				gh.ExpectUser(writer, gh.User{ID: 7, Login: "octocat"}),
				gh.ExpectRole(writer, kb.FullName, gh.RoleWrite),
				gh.ExpectRole(writer, "ours/a", gh.RoleRead),
				gh.ExpectRole(writer, "ours/b", gh.RoleTriage),
				gh.ExpectRole(writer, "ours/c", gh.RoleMaintain),
				gh.ExpectRole(writer, "theirs/kb", gh.RoleAdmin))
			got, err := fake.UserRepositories(t.Context(), testCase.token)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("got %v, want %v", err, testCase.wantErr)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("repositories (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRoleText(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		role    gh.Role
		text    string
		wantErr bool
	}{
		{name: "none", role: gh.RoleNone, text: "none"},
		{name: "read", role: gh.RoleRead, text: "read"},
		{name: "triage", role: gh.RoleTriage, text: "triage"},
		{name: "write", role: gh.RoleWrite, text: "write"},
		{name: "maintain", role: gh.RoleMaintain, text: "maintain"},
		{name: "admin", role: gh.RoleAdmin, text: "admin"},
		{name: "out of range", role: gh.RoleAdmin + 1, text: "Role(6)", wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.role.String(); got != testCase.text {
				t.Errorf("String: got %q, want %q", got, testCase.text)
			}
			raw, err := testCase.role.MarshalText()
			if (err != nil) != testCase.wantErr {
				t.Fatalf("MarshalText: got %v, want error %t", err, testCase.wantErr)
			}
			if testCase.wantErr {
				return
			}
			var got gh.Role
			if err := got.UnmarshalText(raw); err != nil || got != testCase.role {
				t.Errorf("UnmarshalText(%q): got %v, %v, want %v", raw, got, err, testCase.role)
			}
		})
	}

	t.Run("unknown name", func(t *testing.T) {
		t.Parallel()
		var r gh.Role
		if err := r.UnmarshalText([]byte("owner")); err == nil {
			t.Error("got no error, want one")
		}
	})
}
