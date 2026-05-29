import { apiClient, type ApiClient } from './client';
import { clearAuthToken, readAuthToken, writeAuthToken } from './token-store';

export interface AuthSessionUser {
	id: string;
	username: string;
	displayName: string;
	avatarUrl?: string;
	role: string;
}

export interface AuthSession {
	id: string;
	expiresAt: string;
}

export interface AuthSessionResponse {
	authDisabled?: boolean;
	user?: AuthSessionUser;
	session?: AuthSession;
	csrfToken?: string;
	sessionToken?: string;
	error?: string;
	preferences?: { autoSkipIntros?: boolean; posterSize?: 'S' | 'M' | 'L' };
	[key: string]: unknown;
}

export interface ClientBootstrapAuthInfo {
	required?: boolean;
	bootstrapAllowed?: boolean;
	defaultUsername?: string;
	bootstrapEndpoint?: string;
}

export interface ClientBootstrapResponse {
	auth?: ClientBootstrapAuthInfo;
	profiles?: Array<{
		id?: string;
		name?: string;
	}>;
	[key: string]: unknown;
}

export interface LoginRequest {
	username: string;
	password: string;
}

export interface BootstrapRequest extends LoginRequest {
	displayName?: string;
}

export interface UpdatePasswordRequest {
	password: string;
}

export interface UserAccount {
	id: string;
	username: string;
	displayName: string;
	avatarUrl?: string;
	role: string;
	createdAt?: string;
}

export interface ListUsersResponse {
	users?: UserAccount[];
}

export interface CreateUserRequest {
	username: string;
	password: string;
	displayName?: string;
	role: 'admin' | 'standard';
}

export interface UpdateUserRequest {
	displayName: string;
	avatarUrl?: string;
}

export async function getAuthSession(client: ApiClient = apiClient): Promise<AuthSessionResponse> {
	return client.request<AuthSessionResponse>('/api/auth/session');
}

export async function getAuthSessionIfAvailable(
	client: ApiClient = apiClient
): Promise<AuthSessionResponse | null> {
	// Avoid probing protected session routes when there is no local auth token.
	// This keeps signed-out public pages free from expected 401 network noise.
	if (!readAuthToken()) return null;
	return getAuthSession(client);
}

export async function getClientBootstrap(client: ApiClient = apiClient): Promise<ClientBootstrapResponse> {
	return client.request<ClientBootstrapResponse>('/api/client/bootstrap');
}

export async function login(
	payload: LoginRequest,
	client: ApiClient = apiClient
): Promise<AuthSessionResponse> {
	const response = await client.send<AuthSessionResponse, LoginRequest>('/api/auth/login', payload, 'POST');
	const token = String(response.sessionToken ?? '').trim();
	if (token) writeAuthToken(token);
	return response;
}

export async function bootstrapAccount(
	payload: BootstrapRequest,
	client: ApiClient = apiClient
): Promise<AuthSessionResponse> {
	const response = await client.send<AuthSessionResponse, BootstrapRequest>(
		'/api/auth/bootstrap',
		payload,
		'POST'
	);
	const token = String(response.sessionToken ?? '').trim();
	if (token) writeAuthToken(token);
	return response;
}

export async function logout(client: ApiClient = apiClient): Promise<{ status: string }> {
	try {
		return await client.send<{ status: string }, Record<string, never>>('/api/auth/logout', {}, 'POST');
	} finally {
		clearAuthToken();
		// Wipe the offline caches so a different user signing into the same
		// browser doesn't inherit the previous session's library data.
		// We invalidate the SWR layer asynchronously (errors are swallowed —
		// logout must not be blocked on cleanup).
		void purgeOfflineCaches();
	}
}

/**
 * Best-effort: clear every layer that might hold previously-authenticated
 * data — the in-memory + IDB SWR cache, the service worker's stale-while-
 * revalidate cache, and any other Cache API store this build owns.
 *
 * Failures are silent. Logout still completes, and the next read pulls
 * fresh authenticated data from the server.
 */
async function purgeOfflineCaches(): Promise<void> {
	try {
		const { invalidateSwrPrefix } = await import('./cache/swr-cache.js');
		await invalidateSwrPrefix('');
	} catch { /* swr-cache module unavailable in this build */ }

	try {
		if (typeof caches === 'undefined') return;
		const keys = await caches.keys();
		await Promise.all(keys.filter((k) => k.startsWith('xuva-')).map((k) => caches.delete(k)));
	} catch { /* Cache API blocked by browser settings */ }
}

export async function getUsers(client: ApiClient = apiClient): Promise<ListUsersResponse> {
	return client.request<ListUsersResponse>('/api/users');
}

export async function createUser(
	payload: CreateUserRequest,
	client: ApiClient = apiClient
): Promise<{ user: UserAccount }> {
	return client.send<{ user: UserAccount }, CreateUserRequest>('/api/users', payload, 'POST');
}

export async function deleteUser(
	userID: string,
	client: ApiClient = apiClient
): Promise<{ status: string }> {
	return client.send<{ status: string }, Record<string, never>>(
		`/api/users/${encodeURIComponent(userID)}`,
		{},
		'DELETE'
	);
}

export async function updateUser(
	userID: string,
	payload: UpdateUserRequest,
	client: ApiClient = apiClient
): Promise<{ user: UserAccount }> {
	return client.send<{ user: UserAccount }, UpdateUserRequest>(
		`/api/users/${encodeURIComponent(userID)}`,
		payload,
		'PATCH'
	);
}

export async function updateUserPassword(
	userID: string,
	password: string,
	client: ApiClient = apiClient
): Promise<{ status: string }> {
	return client.send<{ status: string }, UpdatePasswordRequest>(
		`/api/users/${encodeURIComponent(userID)}/password`,
		{ password },
		'POST'
	);
}
