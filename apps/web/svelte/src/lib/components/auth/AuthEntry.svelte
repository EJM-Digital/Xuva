<script lang="ts">
	import { onMount } from 'svelte';
	import {
		bootstrapAccount,
		getAuthSessionIfAvailable,
		getClientBootstrap,
		login
	} from '$lib/api/auth';
	import { updateSettings } from '$lib/api/operator';
	import { saveLibrary, startLibraryScan } from '$lib/api/setup';
	import { ApiClientError } from '$lib/api/client';
	import Logo from '$lib/components/Logo.svelte';

	type AuthMode = 'signin' | 'bootstrap';
	type BootstrapStep = 'language' | 'server' | 'account' | 'libraries';

	let { preferredMode = 'auto' }: { preferredMode?: 'auto' | 'signin' | 'bootstrap' } = $props();

	let isLoading = $state(true);
	let isSubmitting = $state(false);
	let mode = $state<AuthMode>('signin');
	let bootstrapStep = $state<BootstrapStep>('language');
	let statusMessage = $state('');
	let errorMessage = $state('');

	let username = $state('');
	let displayName = $state('');
	let bootstrapRole = $state<'admin' | 'standard'>('admin');
	let password = $state('');
	let confirmPassword = $state('');

	let language = $state('en');
	let serverName = $state('Xuva');
	let moviesPath = $state('');
	let tvPath = $state('');
	let moviesName = $state('Movies');
	let tvName = $state('TV Shows');
	let runScanAfterSave = $state(true);

	const bootstrapStepNumber = $derived.by(() => {
		if (bootstrapStep === 'language') return 1;
		if (bootstrapStep === 'server') return 2;
		if (bootstrapStep === 'account') return 3;
		return 4;
	});

	const bootstrapStepLabel = $derived.by(() => {
		if (bootstrapStep === 'language') return 'Language';
		if (bootstrapStep === 'server') return 'Server name';
		if (bootstrapStep === 'account') return 'Account';
		return 'Library paths';
	});

	onMount(() => {
		void initialize();
	});

	async function initialize(): Promise<void> {
		isLoading = true;
		errorMessage = '';
		statusMessage = '';

		try {
			const bootstrap = await getClientBootstrap();
			const auth = bootstrap.auth || {};
			const path = typeof window !== 'undefined' ? window.location.pathname || '/' : '/';
			const wantsSetupQuery = new URL(window.location.href).searchParams.get('setup') === '1';
			const wantsSetupPath = path.startsWith('/setup-wizard');
			const wantsSetup =
				preferredMode === 'bootstrap' ||
				(preferredMode === 'auto' && (wantsSetupPath || wantsSetupQuery));
			mode = wantsSetup && auth.bootstrapAllowed ? 'bootstrap' : 'signin';
			bootstrapStep = 'language';
			username = asText(auth.defaultUsername) || username;
			serverName = asText((bootstrap as { server?: { name?: unknown } })?.server?.name) || 'Xuva';

			if (wantsSetupPath && !auth.bootstrapAllowed) {
				window.location.replace('/signin');
				return;
			}

			const session = await getAuthSessionIfAvailable().catch((error: unknown) => {
				if (isApiStatus(error, 401)) return null;
				throw error;
			});
			if (session?.user) {
				window.location.href = '/';
				return;
			}
		} catch {
			// Server may be temporarily unreachable — fall through to the sign-in form.
			// Meaningful errors will surface when the user actually submits.
		} finally {
			isLoading = false;
		}
	}

	async function submit(): Promise<void> {
		if (isSubmitting) return;
		errorMessage = '';
		statusMessage = '';

		if (mode === 'signin') {
			await submitSignIn();
			return;
		}

		if (bootstrapStep === 'language') {
			saveLanguageChoice();
			bootstrapStep = 'server';
			return;
		}
		if (bootstrapStep === 'server') {
			const serverNameValue = asText(serverName);
			if (!serverNameValue) {
				errorMessage = 'Enter a server name.';
				return;
			}
			if (serverNameValue.length > 50) {
				errorMessage = 'Server name must be 50 characters or fewer.';
				return;
			}
			bootstrapStep = 'account';
			return;
		}
		if (bootstrapStep === 'account') {
			await submitBootstrapAccount();
			return;
		}
		await finishBootstrap(false);
	}

	function saveLanguageChoice(): void {
		const value = asText(language) || 'en';
		try {
			localStorage.setItem('xuva.language', value);
		} catch {
			// Ignore storage failures in restricted browser contexts.
		}
		if (typeof document !== 'undefined') {
			document.documentElement.lang = value;
		}
	}

	async function submitSignIn(): Promise<void> {
		const usernameValue = asText(username);
		const passwordValue = asText(password);

		if (!usernameValue) {
			errorMessage = 'Enter your username.';
			return;
		}
		if (!passwordValue) {
			errorMessage = 'Enter your password.';
			return;
		}

		isSubmitting = true;
		try {
			await login({ username: usernameValue, password: passwordValue });
			window.location.href = '/';
		} catch (error) {
			errorMessage = formatAuthError(error, 'Sign-in failed.');
		} finally {
			isSubmitting = false;
		}
	}

	async function submitBootstrapAccount(): Promise<void> {
		const usernameValue = asText(username);
		const passwordValue = asText(password);
		const displayNameValue = asText(displayName);
		const serverNameValue = asText(serverName);
		const bootstrapRoleValue = bootstrapRole;

		if (!usernameValue) { errorMessage = 'Enter a username.'; return; }
		if (!displayNameValue) { errorMessage = 'Enter a display name.'; return; }
		if (bootstrapRoleValue !== 'admin') {
			errorMessage = 'The first account must be an admin account.';
			return;
		}
		if (!passwordValue) { errorMessage = 'Enter a password.'; return; }
		if (passwordValue !== confirmPassword) { errorMessage = 'Passwords do not match.'; return; }

		isSubmitting = true;
		try {
			await bootstrapAccount({
				username: usernameValue,
				password: passwordValue,
				displayName: displayNameValue
			});

			let serverNameSaved = false;
			try {
				await updateSettings({ serverName: serverNameValue });
				serverNameSaved = true;
			} catch {
				statusMessage =
					'Admin account created. Server name could not be saved right now. You can update it in Settings > General.';
			}

			password = '';
			confirmPassword = '';
			bootstrapRole = 'admin';
			bootstrapStep = 'libraries';
			if (serverNameSaved) {
				statusMessage = 'Admin account created. Server name saved. Add library paths now or skip for later.';
			}
		} catch (error) {
			errorMessage = formatAuthError(error, 'Account setup failed.');
		} finally {
			isSubmitting = false;
		}
	}

	async function finishBootstrap(skipLibraries: boolean): Promise<void> {
		if (isSubmitting) return;
		errorMessage = '';
		statusMessage = '';

		const moviesPathValue = asText(moviesPath);
		const tvPathValue = asText(tvPath);
		const moviesNameValue = asText(moviesName) || 'Movies';
		const tvNameValue = asText(tvName) || 'TV Shows';

		isSubmitting = true;
		try {
			if (!skipLibraries) {
				if (moviesPathValue) {
					const created = await saveLibrary({ name: moviesNameValue, kind: 'movies', path: moviesPathValue });
					if (runScanAfterSave && created.id) await startLibraryScan(created.id);
				}
				if (tvPathValue) {
					const created = await saveLibrary({ name: tvNameValue, kind: 'tv', path: tvPathValue });
					if (runScanAfterSave && created.id) await startLibraryScan(created.id);
				}
			}
			window.location.href = '/';
		} catch (error) {
			errorMessage = formatAuthError(error, 'Initial library setup failed.');
		} finally {
			isSubmitting = false;
		}
	}

	function asText(value: unknown): string {
		return String(value ?? '').trim();
	}

	function isApiStatus(error: unknown, expectedStatus: number): boolean {
		if (error instanceof ApiClientError) return error.status === expectedStatus;
		if (typeof error !== 'object' || !error) return false;
		return Number((error as { status?: unknown }).status) === expectedStatus;
	}

	function formatAuthError(error: unknown, fallback: string): string {
		if (error instanceof ApiClientError) return error.userMessage || error.message || fallback;
		if (error instanceof Error) return error.message || fallback;
		return fallback;
	}
</script>

<div class="relative flex min-h-screen flex-col items-center justify-center bg-background px-6 py-16">
	<!-- Radial background glow -->
	<div
		aria-hidden="true"
		class="pointer-events-none absolute inset-0 -z-10"
		style="background: radial-gradient(ellipse at 25% 15%, oklch(0.62 0.22 285 / 0.35), transparent 50%), radial-gradient(ellipse at 80% 85%, oklch(0.72 0.16 255 / 0.28), transparent 50%), radial-gradient(ellipse at 65% 5%, oklch(0.68 0.20 300 / 0.22), transparent 40%);"
	></div>
	<div class="grain pointer-events-none absolute inset-0 -z-10"></div>

	<!-- Card -->
	<div class="hairline w-full max-w-sm rounded-3xl bg-surface/60 px-8 py-10 shadow-elev backdrop-blur-xl">
		<!-- Logo + wordmark -->
		<div class="mb-8 flex flex-col items-center gap-3">
			<a href="/" aria-label="Xuva home">
				<Logo />
			</a>

			{#if mode === 'bootstrap'}
				<!-- Step indicator -->
				<div class="mt-2 flex items-center gap-1.5">
					{#each [1, 2, 3, 4] as step (step)}
						<span
							class={`h-1 rounded-full transition-all duration-300 ${
								step === bootstrapStepNumber
									? 'w-6 bg-primary-glow shadow-glow'
									: step < bootstrapStepNumber
										? 'w-3 bg-primary-glow/60'
										: 'w-3 bg-foreground/15'
							}`}
						></span>
					{/each}
				</div>
				<div class="text-center">
					<div class="text-[10px] font-semibold uppercase tracking-[0.3em] text-primary-glow">
						Setup · Step {bootstrapStepNumber} of 4
					</div>
					<h1 class="font-serif-display mt-1 text-2xl tracking-tight">
						{bootstrapStepLabel}
					</h1>
				</div>
			{:else}
				<div class="text-center">
					<h1 class="font-serif-display text-2xl tracking-tight">
						Sign in to <em>Xuva</em>
					</h1>
					<p class="mt-1.5 text-sm text-muted-foreground">
						Use your account to open your library.
					</p>
				</div>
			{/if}
		</div>

		{#if isLoading}
			<p class="text-center text-sm text-muted-foreground">Checking authentication status…</p>
		{:else if mode === 'signin'}
			<form class="space-y-4" onsubmit={(e) => { e.preventDefault(); void submit(); }}>
				<div class="space-y-1.5">
					<label for="username" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
						Username
					</label>
					<input
						id="username"
						bind:value={username}
						autocomplete="username"
						class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70"
					/>
				</div>
				<div class="space-y-1.5">
					<label for="password" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
						Password
					</label>
					<input
						id="password"
						type="password"
						bind:value={password}
						autocomplete="current-password"
						class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70"
					/>
				</div>

				{#if errorMessage}
					<p class="rounded-xl bg-destructive/10 px-4 py-3 text-sm text-destructive">
						{errorMessage}
					</p>
				{/if}

				<button
					type="submit"
					disabled={isSubmitting}
					class="mt-2 w-full rounded-full bg-gradient-primary py-3 text-sm font-semibold text-white shadow-glow ring-1 ring-white/20 transition hover:brightness-110 disabled:cursor-not-allowed disabled:opacity-60"
				>
					{isSubmitting ? 'Signing in…' : 'Sign in'}
				</button>
			</form>
		{:else}
			<form class="space-y-4" onsubmit={(e) => { e.preventDefault(); void submit(); }}>
				{#if bootstrapStep === 'language'}
					<p class="text-sm text-muted-foreground">
						Choose your preferred language for this browser session.
					</p>
					<div class="space-y-1.5">
						<label for="language" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
							Language
						</label>
						<select
							id="language"
							bind:value={language}
							class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none focus:border-primary/60 focus:bg-background/70"
						>
							<option value="en">English</option>
							<option value="es">Spanish</option>
							<option value="fr">French</option>
							<option value="de">German</option>
							<option value="it">Italian</option>
							<option value="pt">Portuguese</option>
						</select>
					</div>
				{:else if bootstrapStep === 'server'}
					<p class="text-sm text-muted-foreground">
						Choose the server name clients will see on your home network.
					</p>
					<div class="space-y-1.5">
						<label for="server-name" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
							Server name
						</label>
						<input
							id="server-name"
							bind:value={serverName}
							maxlength="50"
							placeholder="Xuva"
							class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70"
						/>
					</div>
				{:else if bootstrapStep === 'account'}
					<p class="text-sm text-muted-foreground">Create the first account for this server.</p>
					<div class="space-y-1.5">
						<label for="account-type" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
							Account type
						</label>
						<select
							id="account-type"
							bind:value={bootstrapRole}
							class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none focus:border-primary/60 focus:bg-background/70"
						>
							<option value="admin">Admin (full access)</option>
							<option value="standard">User (media only)</option>
						</select>
					</div>
					{#if bootstrapRole === 'standard'}
						<p class="rounded-xl bg-destructive/10 px-4 py-3 text-sm text-destructive">
							The first account must be Admin so setup and settings remain accessible.
						</p>
					{/if}
					{#each [
						{ id: 'bs-username', label: 'Username', bind: 'username', autocomplete: 'username' },
						{ id: 'bs-display', label: 'Display name', bind: 'displayName', autocomplete: 'name' }
					] as field (field.id)}
						<div class="space-y-1.5">
							<label for={field.id} class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
								{field.label}
							</label>
							{#if field.bind === 'username'}
								<input id={field.id} bind:value={username} autocomplete="username" class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{:else}
								<input id={field.id} bind:value={displayName} autocomplete="name" class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{/if}
						</div>
					{/each}
					<div class="space-y-1.5">
						<label for="bs-password" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
							Password
						</label>
						<input id="bs-password" type="password" bind:value={password} autocomplete="new-password" class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
					</div>
					<div class="space-y-1.5">
						<label for="bs-confirm" class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
							Confirm password
						</label>
						<input id="bs-confirm" type="password" bind:value={confirmPassword} autocomplete="new-password" class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
					</div>
				{:else}
					<p class="text-sm text-muted-foreground">
						Add initial library paths now, or skip and do this later from Settings.
					</p>
					{#each [
						{ id: 'movies-path', label: 'Movies library path (optional)', placeholder: 'D:\\Media\\Movies', bind: 'moviesPath' },
						{ id: 'movies-name', label: 'Movies library name', placeholder: 'Movies', bind: 'moviesName' },
						{ id: 'tv-path', label: 'TV library path (optional)', placeholder: 'D:\\Media\\TV', bind: 'tvPath' },
						{ id: 'tv-name', label: 'TV library name', placeholder: 'TV Shows', bind: 'tvName' }
					] as field (field.id)}
						<div class="space-y-1.5">
							<label for={field.id} class="text-[10px] font-semibold uppercase tracking-[0.28em] text-muted-foreground">
								{field.label}
							</label>
							{#if field.bind === 'moviesPath'}
								<input id={field.id} bind:value={moviesPath} placeholder={field.placeholder} class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{:else if field.bind === 'moviesName'}
								<input id={field.id} bind:value={moviesName} placeholder={field.placeholder} class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{:else if field.bind === 'tvPath'}
								<input id={field.id} bind:value={tvPath} placeholder={field.placeholder} class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{:else}
								<input id={field.id} bind:value={tvName} placeholder={field.placeholder} class="h-11 w-full rounded-xl border border-border bg-background/40 px-4 text-sm outline-none placeholder:text-muted-foreground/60 focus:border-primary/60 focus:bg-background/70" />
							{/if}
						</div>
					{/each}
					<label class="flex cursor-pointer items-center gap-3 text-sm text-muted-foreground">
						<input type="checkbox" bind:checked={runScanAfterSave} class="accent-primary-glow" />
						Start scan after saving library paths
					</label>
				{/if}

				{#if errorMessage}
					<p class="rounded-xl bg-destructive/10 px-4 py-3 text-sm text-destructive">
						{errorMessage}
					</p>
				{/if}
				{#if statusMessage}
					<p class="rounded-xl bg-primary/10 px-4 py-3 text-sm text-primary-glow">
						{statusMessage}
					</p>
				{/if}

				<div class="flex flex-wrap gap-2 pt-1">
					<button
						type="submit"
						disabled={isSubmitting}
						class="flex-1 rounded-full bg-gradient-primary py-3 text-sm font-semibold text-white shadow-glow ring-1 ring-white/20 transition hover:brightness-110 disabled:cursor-not-allowed disabled:opacity-60"
					>
						{#if isSubmitting}
							{bootstrapStep === 'libraries' ? 'Finishing setup…' : 'Saving…'}
						{:else}
							{bootstrapStep === 'language' || bootstrapStep === 'server'
								? 'Continue'
								: bootstrapStep === 'account'
									? 'Create account'
									: 'Finish setup'}
						{/if}
					</button>
					{#if bootstrapStep === 'libraries'}
						<button
							type="button"
							disabled={isSubmitting}
							onclick={() => void finishBootstrap(true)}
							class="hairline rounded-full bg-foreground/[0.04] px-5 py-3 text-sm font-medium text-muted-foreground transition-colors hover:bg-foreground/[0.08] hover:text-foreground disabled:cursor-not-allowed disabled:opacity-60"
						>
							Skip for now
						</button>
					{/if}
				</div>
			</form>
		{/if}
	</div>

	<!-- Footer -->
	<p class="mt-8 text-center text-[11px] uppercase tracking-[0.2em] text-muted-foreground/50">
		Xuva · Your personal media library
	</p>
</div>
