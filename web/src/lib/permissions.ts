/**
 * Fine-grained RBAC for the dashboard: mirrors the backend catalog
 * (internal/rbac) and exposes permission state from /api/admin/users/me.
 */
export type Perm =
  | 'keys:read' | 'keys:read_own' | 'keys:create' | 'keys:update' | 'keys:rotate' | 'keys:delete' | 'keys:limits'
  | 'providers:read' | 'providers:write' | 'providers:test' | 'providers:delete'
  | 'logs:read' | 'logs:read_bodies' | 'analytics:read'
  | 'routing:read' | 'routing:write'
  | 'catalog:read' | 'catalog:write'
  | 'orgs:read' | 'orgs:write'
  | 'users:read' | 'users:write'
  | 'audit:read'
  | 'settings:write'

export const PERM_CATALOG: Perm[] = [
  'keys:read', 'keys:read_own', 'keys:create', 'keys:update', 'keys:rotate', 'keys:delete', 'keys:limits',
  'providers:read', 'providers:write', 'providers:test', 'providers:delete',
  'logs:read', 'logs:read_bodies', 'analytics:read',
  'routing:read', 'routing:write',
  'catalog:read', 'catalog:write',
  'orgs:read', 'orgs:write',
  'users:read', 'users:write',
  'audit:read',
  'settings:write',
]

/** UI grouping for the permissions editor. */
export const PERM_GROUPS: { resource: string; label: string; perms: { perm: Perm; label: string; hint?: string }[] }[] = [
  {
    resource: 'keys', label: 'API Keys',
    perms: [
      { perm: 'keys:read', label: 'View all keys' },
      { perm: 'keys:read_own', label: 'View own keys', hint: 'Keys this user created. Legacy/unassigned keys stay hidden.' },
      { perm: 'keys:create', label: 'Create keys' },
      { perm: 'keys:update', label: 'Rename keys' },
      { perm: 'keys:rotate', label: 'Rotate key secrets', hint: 'Regenerate a key secret; the old one keeps working 24h. Users with only "view own keys" can rotate keys assigned to them.' },
      { perm: 'keys:delete', label: 'Revoke keys' },
      { perm: 'keys:limits', label: 'Edit limits & allowlists' },
    ],
  },
  {
    resource: 'providers', label: 'Providers',
    perms: [
      { perm: 'providers:read', label: 'View providers' },
      { perm: 'providers:write', label: 'Create & edit providers' },
      { perm: 'providers:test', label: 'Test provider credentials' },
      { perm: 'providers:delete', label: 'Delete providers' },
    ],
  },
  {
    resource: 'logs', label: 'Logs & Analytics',
    perms: [
      { perm: 'logs:read', label: 'View request logs' },
      { perm: 'logs:read_bodies', label: 'View request/response bodies', hint: 'Sensitive: full payloads stored in logs.' },
      { perm: 'analytics:read', label: 'View analytics & reports' },
    ],
  },
  {
    resource: 'routing', label: 'Routing',
    perms: [
      { perm: 'routing:read', label: 'View routing rules' },
      { perm: 'routing:write', label: 'Edit routing rules' },
    ],
  },
  {
    resource: 'catalog', label: 'Model Catalog',
    perms: [
      { perm: 'catalog:read', label: 'View catalog' },
      { perm: 'catalog:write', label: 'Sync, aliases & catalog settings' },
    ],
  },
  {
    resource: 'orgs', label: 'Teams / Orgs',
    perms: [
      { perm: 'orgs:read', label: 'View organizations' },
      { perm: 'orgs:write', label: 'Manage organizations & members' },
    ],
  },
  {
    resource: 'users', label: 'Users',
    perms: [
      { perm: 'users:read', label: 'View users' },
      { perm: 'users:write', label: 'Manage users & permissions' },
    ],
  },
  {
    resource: 'system', label: 'System',
    perms: [
      { perm: 'audit:read', label: 'View audit trail' },
      { perm: 'settings:write', label: 'Gateway settings' },
    ],
  },
]

/** Stable module-level holder for the current user's permission set. */
let currentPerms: Perm[] | null = null
let currentRole = ''

export function setCurrentPermissions(perms: Perm[] | null, role: string) {
  currentPerms = perms
  currentRole = role
}

export function currentPermissions(): Perm[] | null {
  return currentPerms
}

/**
 * Can reports whether the current user holds a permission. Admins always
 * pass (mirrors the backend short-circuit). When the permission set is
 * unknown (pre-upgrade backend), fall back to legacy role behavior so the
 * UI never locks out working deployments.
 */
export function can(perm: Perm): boolean {
  if (currentRole === 'admin') return true
  if (currentPerms) return currentPerms.includes(perm)
  return legacyCan(perm)
}

/** Pre-RBAC fallback keyed on the coarse roles (matches the old route guards). */
function legacyCan(perm: Perm): boolean {
  const writeRoles = ['admin', 'member', 'support']
  switch (perm) {
    case 'keys:read':
      // Legacy readonly could see all keys (unguarded GET /keys); the RBAC
      // default scopes readonly to own keys instead — honor the new default
      // here only when a permission set has actually been received.
      return writeRoles.includes(currentRole)
    case 'keys:read_own':
      return true
    case 'keys:create': case 'keys:update': case 'keys:rotate': case 'keys:delete': case 'keys:limits':
    case 'providers:read': case 'providers:write': case 'providers:test': case 'providers:delete':
      return writeRoles.includes(currentRole)
    case 'logs:read': case 'logs:read_bodies': case 'analytics:read':
      // Org-wide request visibility: legacy readonly saw it, the RBAC default
      // scopes readonly to own-keys traffic — same policy as the backend.
      return writeRoles.includes(currentRole)
    case 'catalog:read': case 'orgs:read':
      return true
    case 'routing:read': case 'users:read':
    case 'routing:write': case 'catalog:write': case 'orgs:write':
    case 'users:write': case 'audit:read': case 'settings:write':
      return currentRole === 'admin'
    default:
      return false
  }
}

/** Check several permissions at once (all must hold). */
export function canAll(...perms: Perm[]): boolean {
  return perms.every(can)
}

/** True when the user may view keys at all (all or own). */
export function canSeeKeys(): boolean {
  return can('keys:read') || can('keys:read_own')
}

/** True when the user is restricted to their own keys. */
export function ownKeysOnly(): boolean {
  return !can('keys:read') && can('keys:read_own')
}
