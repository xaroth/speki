package gh_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/xaroth/speki/gh"
	"github.com/xaroth/speki/gh/ghtest"
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
			fake := ghtest.New(t, withKB(), withUsers())
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
			fake := ghtest.New(t, withKB(), withUsers(),
				ghtest.ExpectOrgMembership(writer, "ours", "active"),
				ghtest.ExpectOrgMembership(writer, "invited", "pending"))
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
			fake := ghtest.New(t, withKB(), withUsers(),
				ghtest.ExpectTeamMembership("ours", "editors", "octocat", "active"),
				ghtest.ExpectTeamMembership("ours", "invited", "octocat", "pending"))
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
			fake := ghtest.New(t, ghtest.ExpectPageSize(2),
				ghtest.ExpectInstallation(gh.Installation{ID: 1}, append([]gh.Repository{kb}, repos...)...),
				ghtest.ExpectInstallation(gh.Installation{ID: 2}, gh.Repository{ID: 200, FullName: "theirs/kb"}),
				ghtest.ExpectInstallation(gh.Installation{ID: 3}, gh.Repository{ID: 300, FullName: "theirs/hidden"}),
				ghtest.ExpectUser(writer, gh.User{ID: 7, Login: "octocat"}),
				ghtest.ExpectRole(writer, kb.FullName, gh.RoleWrite),
				ghtest.ExpectRole(writer, "ours/a", gh.RoleRead),
				ghtest.ExpectRole(writer, "ours/b", gh.RoleTriage),
				ghtest.ExpectRole(writer, "ours/c", gh.RoleMaintain),
				ghtest.ExpectRole(writer, "theirs/kb", gh.RoleAdmin))
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
