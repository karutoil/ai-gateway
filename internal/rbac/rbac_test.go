package rbac

import (
	"encoding/json"
	"testing"
)

func TestValidCatalog(t *testing.T) {
	// Every catalog entry must parse as resource:action with known resources.
	knownResources := map[string]bool{
		"keys": true, "providers": true, "logs": true, "analytics": true,
		"routing": true, "catalog": true, "orgs": true, "users": true,
		"audit": true, "settings": true,
	}
	for _, p := range All {
		if !Valid(p) {
			t.Errorf("catalog entry %q not Valid()", p)
		}
		// resource:action shape
		colon := -1
		for i, c := range p {
			if c == ':' {
				colon = i
				break
			}
		}
		if colon <= 0 || colon == len(p)-1 {
			t.Errorf("permission %q not resource:action shaped", p)
			continue
		}
		if !knownResources[p[:colon]] {
			t.Errorf("permission %q uses unknown resource %q", p, p[:colon])
		}
	}
	if Valid("keys:bogus") {
		t.Error("unknown permission must not be Valid")
	}
	if Valid("") {
		t.Error("empty permission must not be Valid")
	}
}

func TestDefaultsMatchLegacyBehavior(t *testing.T) {
	// These assertions pin the role defaults to the pre-RBAC RequireRole
	// behavior. If one fails, either the default set drifted or a route was
	// re-guarded — the difference must be intentional either way.
	cases := []struct {
		role string
		perm string
		want bool
	}{
		// keys writable by admin/support/member (legacy admin,member,support)
		{RoleAdmin, PermKeysCreate, true},
		{RoleSupport, PermKeysCreate, true},
		{RoleMember, PermKeysCreate, true},
		{RoleReadonly, PermKeysCreate, false},
		// providers writable by admin/support/member
		{RoleMember, PermProvidersWrite, true},
		{RoleSupport, PermProvidersWrite, true},
		// provider test includes readonly (legacy listed readonly)
		{RoleReadonly, PermProvidersTest, true},
		// routing/catalog/orgs/settings/users writes AND sensitive reads:
		// admin only (legacy RequireRole("admin") on those routes)
		{RoleSupport, PermRoutingWrite, false},
		{RoleMember, PermRoutingWrite, false},
		{RoleSupport, PermRoutingRead, false},
		{RoleMember, PermRoutingRead, false},
		{RoleReadonly, PermRoutingRead, false},
		{RoleSupport, PermUsersRead, false},
		{RoleMember, PermUsersRead, false},
		{RoleReadonly, PermUsersRead, false},
		{RoleSupport, PermCatalogWrite, false},
		{RoleMember, PermCatalogWrite, false},
		{RoleSupport, PermOrgsWrite, false},
		{RoleMember, PermOrgsWrite, false},
		{RoleSupport, PermUsersWrite, false},
		{RoleMember, PermUsersWrite, false},
		{RoleSupport, PermSettingsWrite, false},
		{RoleReadonly, PermUsersWrite, false},
		// audit read: admin only (legacy RequireRole("admin"))
		{RoleSupport, PermAuditRead, false},
		{RoleMember, PermAuditRead, false},
		// reads for everyone including readonly...
		{RoleReadonly, PermProvidersRead, true},
		{RoleReadonly, PermProvidersDelete, false},
		// ...except the all-keys view and org-wide request data: readonly is
		// scoped to own keys/traffic (the user-requested restricted default;
		// admins grant keys:read / logs:read / analytics:read explicitly).
		{RoleReadonly, PermKeysRead, false},
		{RoleReadonly, PermKeysReadOwn, true},
		{RoleReadonly, PermLogsRead, false},
		{RoleReadonly, PermAnalyticsRead, false},
		// every role can see its own keys at minimum
		{RoleMember, PermKeysReadOwn, true},
		{RoleSupport, PermKeysReadOwn, true},
		{RoleReadonly, PermKeysReadOwn, true},
	}
	for _, c := range cases {
		if got := Has(Defaults(c.role), c.perm); got != c.want {
			t.Errorf("Defaults(%q)[%q] = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestEffectiveGrantsAndRevokes(t *testing.T) {
	// Grant elevates: readonly + keys:create can create.
	got := Effective(RoleReadonly, map[string]bool{PermKeysCreate: true})
	if !Has(got, PermKeysCreate) {
		t.Error("grant should elevate readonly to keys:create")
	}
	// Base perms survive alongside the grant (read_own is the readonly default).
	if !Has(got, PermKeysReadOwn) {
		t.Error("grant must not remove base perms")
	}
	// Revoke removes: member − keys:create.
	got = Effective(RoleMember, map[string]bool{PermKeysCreate: false})
	if Has(got, PermKeysCreate) {
		t.Error("revoke should remove keys:create from member")
	}
	if !Has(got, PermKeysRead) {
		t.Error("revoke must not remove other base perms")
	}
	// Combined grant+revoke on different perms.
	got = Effective(RoleSupport, map[string]bool{PermAuditRead: true, PermKeysDelete: false})
	if !Has(got, PermAuditRead) || Has(got, PermKeysDelete) {
		t.Errorf("combined overrides wrong: %v", Sorted(got))
	}
	// Unknown permission entries are ignored (fail closed).
	got = Effective(RoleMember, map[string]bool{"bogus:perm": true, PermKeysRead: false})
	if Has(got, "bogus:perm") {
		t.Error("unknown permission must never be granted")
	}
	// Admin short-circuits: revokes have no effect.
	got = Effective(RoleAdmin, map[string]bool{PermKeysDelete: false, PermUsersWrite: false})
	for _, p := range All {
		if !Has(got, p) {
			t.Errorf("admin lost %q despite revoke", p)
		}
	}
	// Empty/nil overrides = pure defaults.
	got = Effective(RoleMember, nil)
	if !Has(got, PermKeysCreate) || Has(got, PermUsersWrite) {
		t.Errorf("nil overrides should equal defaults: %v", Sorted(got))
	}
}

func TestUnknownRoleFailsClosed(t *testing.T) {
	got := Effective("bogus-role", map[string]bool{PermKeysRead: true})
	// Even with an explicit grant the unknown role gets only what was granted
	// — no inherited defaults.
	if !Has(got, PermKeysRead) {
		t.Error("explicit grant should still apply for unknown role")
	}
	if Has(got, PermKeysCreate) {
		t.Error("unknown role must have no default perms")
	}
	if len(Defaults("bogus-role")) != 0 {
		t.Error("unknown role defaults must be empty")
	}
}

func TestSortedStableJSON(t *testing.T) {
	b, err := json.Marshal(Sorted(Effective(RoleReadonly, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var back []string
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(back); i++ {
		if back[i-1] > back[i] {
			t.Fatalf("not sorted: %v", back)
		}
	}
}
