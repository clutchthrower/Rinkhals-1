<script lang="ts">
	import { onMount, onDestroy } from "svelte";
	import {
		Boxes,
		Play,
		Square,
		RotateCw,
		Sliders,
		Cpu,
		HardDrive,
		Search,
		X,
		RotateCcw,
		Loader2,
		AlertCircle
	} from "lucide-svelte";

	type Property = {
		key: string;
		display: string;
		type: string;
		options?: string[];
		default: string;
		value: string;
		overridden: boolean;
	};

	type App = {
		id: string;
		name: string;
		description: string;
		version: string;
		source: "system" | "user";
		enabled: boolean;
		running: boolean;
		requirements?: { memory?: number; cpu?: number };
		properties: Property[];
	};

	const apiHost = import.meta.env.DEV ? "http://localhost:8080" : "";

	let apps = $state<App[]>([]);
	let loading = $state(true);
	let error = $state("");

	let searchQuery = $state("");
	let stateFilter = $state<"all" | "enabled" | "disabled" | "running">("all");
	let sourceFilter = $state<"all" | "system" | "user">("all");

	let busyApps = $state<Set<string>>(new Set());
	let configureApp = $state<App | null>(null);
	let pendingConfig = $state<Record<string, string>>({});
	let configSaving = $state(false);
	let toast = $state<{ text: string; tone: "ok" | "err" } | null>(null);

	let pollHandle: ReturnType<typeof setInterval> | undefined;

	async function fetchApps() {
		try {
			const res = await fetch(`${apiHost}/api/apps`);
			if (!res.ok) throw new Error(`HTTP ${res.status}`);
			const fresh: App[] = await res.json();
			apps = fresh;
			error = "";
		} catch (e: any) {
			error = e.message || String(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		fetchApps();
		pollHandle = setInterval(fetchApps, 5000);
	});

	onDestroy(() => {
		if (pollHandle) clearInterval(pollHandle);
	});

	function showToast(text: string, tone: "ok" | "err" = "ok") {
		toast = { text, tone };
		setTimeout(() => {
			toast = null;
		}, 2500);
	}

	function markBusy(id: string, on: boolean) {
		const next = new Set(busyApps);
		if (on) next.add(id);
		else next.delete(id);
		busyApps = next;
	}

	async function callApp(id: string, path: string, init: RequestInit = {}) {
		markBusy(id, true);
		try {
			const res = await fetch(`${apiHost}/api/apps/${id}${path}`, {
				...init,
				headers: { "Content-Type": "application/json", ...(init.headers || {}) }
			});
			if (!res.ok) throw new Error(`HTTP ${res.status}`);
			const data = await res.json().catch(() => ({}));
			if (data && data.success === false) {
				throw new Error(data.output || "Operation failed");
			}
			await fetchApps();
			return data;
		} catch (e: any) {
			showToast(e.message || "Operation failed", "err");
			throw e;
		} finally {
			markBusy(id, false);
		}
	}

	async function toggleEnabled(app: App) {
		try {
			await callApp(app.id, app.enabled ? "/disable" : "/enable", { method: "POST" });
			showToast(`${app.name} ${app.enabled ? "disabled" : "enabled"}`);
		} catch {
			// toast already shown
		}
	}

	async function runAction(app: App, action: "start" | "stop" | "restart") {
		try {
			await callApp(app.id, "/action", {
				method: "POST",
				body: JSON.stringify({ action })
			});
			showToast(`${app.name}: ${action} sent`);
		} catch {}
	}

	function openConfigure(app: App) {
		configureApp = app;
		pendingConfig = {};
	}

	function closeConfigure() {
		configureApp = null;
		pendingConfig = {};
	}

	function pendingValue(prop: Property): string {
		return pendingConfig[prop.key] ?? prop.value;
	}

	function hasPendingChanges(): boolean {
		return Object.keys(pendingConfig).length > 0;
	}

	async function saveConfig() {
		if (!configureApp || !hasPendingChanges()) return;
		configSaving = true;
		try {
			const res = await fetch(`${apiHost}/api/apps/${configureApp.id}/config`, {
				method: "PUT",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify(pendingConfig)
			});
			const data = await res.json().catch(() => ({}));
			if (!res.ok || data.success === false) {
				throw new Error(data.output || `HTTP ${res.status}`);
			}
			showToast("Configuration saved");
			await fetchApps();
			const refreshed = apps.find((a) => a.id === configureApp!.id);
			if (refreshed) configureApp = refreshed;
			pendingConfig = {};
		} catch (e: any) {
			showToast(e.message || "Save failed", "err");
		} finally {
			configSaving = false;
		}
	}

	async function resetProperty(prop: Property) {
		if (!configureApp) return;
		try {
			const res = await fetch(`${apiHost}/api/apps/${configureApp.id}/config/${prop.key}`, {
				method: "DELETE"
			});
			if (!res.ok) throw new Error(`HTTP ${res.status}`);
			showToast(`${prop.display || prop.key} reset to default`);
			const fresh = { ...pendingConfig };
			delete fresh[prop.key];
			pendingConfig = fresh;
			await fetchApps();
			const refreshed = apps.find((a) => a.id === configureApp!.id);
			if (refreshed) configureApp = refreshed;
		} catch (e: any) {
			showToast(e.message || "Reset failed", "err");
		}
	}

	async function clearAllOverrides() {
		if (!configureApp) return;
		try {
			const res = await fetch(`${apiHost}/api/apps/${configureApp.id}/config`, {
				method: "DELETE"
			});
			if (!res.ok) throw new Error(`HTTP ${res.status}`);
			showToast("All overrides cleared");
			pendingConfig = {};
			await fetchApps();
			const refreshed = apps.find((a) => a.id === configureApp!.id);
			if (refreshed) configureApp = refreshed;
		} catch (e: any) {
			showToast(e.message || "Clear failed", "err");
		}
	}

	let visibleApps = $derived(
		apps.filter((a) => {
			if (searchQuery) {
				const q = searchQuery.toLowerCase();
				if (
					!a.id.toLowerCase().includes(q) &&
					!a.name.toLowerCase().includes(q) &&
					!(a.description || "").toLowerCase().includes(q)
				) {
					return false;
				}
			}
			if (stateFilter === "enabled" && !a.enabled) return false;
			if (stateFilter === "disabled" && a.enabled) return false;
			if (stateFilter === "running" && !a.running) return false;
			if (sourceFilter !== "all" && a.source !== sourceFilter) return false;
			return true;
		})
	);

	function statusPill(app: App): { label: string; classes: string } {
		if (!app.enabled) {
			return {
				label: "Disabled",
				classes: "bg-surface-accent text-ink-faint border-line-soft"
			};
		}
		if (app.running) {
			return {
				label: "Running",
				classes: "bg-brand-soft text-brand border-brand/30"
			};
		}
		return {
			label: "Stopped",
			classes: "bg-surface-warm text-accent-hover border-accent/30"
		};
	}
</script>

<svelte:head>
	<title>Apps - Rinkhals</title>
</svelte:head>

<div class="space-y-6 max-w-7xl mx-auto w-full">
	<header class="flex flex-col xl:flex-row xl:items-end xl:justify-between gap-4 pb-4 border-b border-line-soft">
		<div>
			<p class="text-xs uppercase tracking-wider text-ink-faint font-medium">Apps</p>
			<h2 class="text-3xl font-semibold text-ink mt-1 tracking-tight flex items-center gap-2">
				<Boxes size={26} class="text-brand" />
				Installed apps
			</h2>
			<p class="text-ink-muted text-sm mt-2">Enable, start and configure the apps shipped with Rinkhals.</p>
		</div>
		<div class="flex flex-wrap items-center gap-2">
			<div class="relative">
				<Search class="absolute left-3 top-1/2 -translate-y-1/2 text-ink-faint" size={15} />
				<input
					bind:value={searchQuery}
					type="text"
					placeholder="Search..."
					class="bg-canvas text-ink rounded-lg pl-9 pr-3 py-2 border border-line-soft focus:outline-none focus:border-brand focus:ring-2 focus:ring-brand/20 text-sm w-52"
				/>
			</div>
			<select
				bind:value={stateFilter}
				class="bg-canvas text-ink border border-line-soft rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-brand focus:ring-2 focus:ring-brand/20"
			>
				<option value="all">All states</option>
				<option value="enabled">Enabled</option>
				<option value="disabled">Disabled</option>
				<option value="running">Running</option>
			</select>
			<select
				bind:value={sourceFilter}
				class="bg-canvas text-ink border border-line-soft rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-brand focus:ring-2 focus:ring-brand/20"
			>
				<option value="all">All sources</option>
				<option value="system">System</option>
				<option value="user">User</option>
			</select>
		</div>
	</header>

	{#if error}
		<div class="bg-surface-accent border border-coral/40 rounded-xl px-4 py-3 text-coral text-sm flex items-center gap-2">
			<AlertCircle size={16} />
			{error}
		</div>
	{/if}

	{#if loading && apps.length === 0}
		<div class="text-ink-faint animate-pulse">Loading apps...</div>
	{:else if visibleApps.length === 0}
		<div class="text-ink-faint italic">No apps match the current filters.</div>
	{:else}
		<div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
			{#each visibleApps as app (app.id)}
				{@const pill = statusPill(app)}
				{@const isBusy = busyApps.has(app.id)}
				<div class="bg-canvas border border-line-soft rounded-xl p-5 flex flex-col gap-3 hover:border-brand/40 transition-colors">
					<div class="flex items-start justify-between gap-3">
						<div class="min-w-0">
							<div class="flex items-center gap-2 flex-wrap">
								<h3 class="text-base font-semibold text-ink truncate">{app.name}</h3>
								{#if app.version}
									<span class="text-[11px] text-ink-faint font-mono">v{app.version}</span>
								{/if}
								<span class="text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded {app.source === 'user' ? 'bg-surface-warm text-accent-hover border border-accent/30' : 'bg-surface text-ink-muted border border-line-soft'}">
									{app.source}
								</span>
							</div>
							<p class="text-[11px] text-ink-faint font-mono mt-0.5">{app.id}</p>
						</div>

						<!-- Enable toggle -->
						<button
							type="button"
							onclick={() => toggleEnabled(app)}
							disabled={isBusy}
							title={app.enabled ? "Disable" : "Enable"}
							aria-label="Toggle enabled"
							class="shrink-0 relative inline-flex h-6 w-11 items-center rounded-full transition-colors disabled:opacity-50 {app.enabled ? 'bg-brand' : 'bg-surface border border-line-soft'}"
						>
							<span class="inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform {app.enabled ? 'translate-x-6' : 'translate-x-1'}"></span>
						</button>
					</div>

					{#if app.description}
						<p class="text-sm text-ink-muted line-clamp-2">{app.description}</p>
					{/if}

					<div class="flex items-center gap-2 flex-wrap">
						<span class="px-2.5 py-1 rounded-full text-[11px] font-semibold uppercase tracking-wider border {pill.classes}">
							{pill.label}
						</span>
						{#if app.requirements?.memory}
							<span class="inline-flex items-center gap-1 text-[11px] text-ink-faint">
								<HardDrive size={11} /> {app.requirements.memory} MB
							</span>
						{/if}
						{#if app.requirements?.cpu}
							<span class="inline-flex items-center gap-1 text-[11px] text-ink-faint">
								<Cpu size={11} /> {app.requirements.cpu}%
							</span>
						{/if}
					</div>

					<div class="flex items-center gap-2 mt-auto pt-3 border-t border-line-soft">
						{#if app.enabled}
							{#if app.running}
								<button
									type="button"
									onclick={() => runAction(app, "stop")}
									disabled={isBusy}
									class="flex items-center gap-1.5 px-2.5 py-1.5 bg-surface hover:bg-surface-warm border border-line-soft rounded-lg text-xs font-medium text-ink-2 disabled:opacity-50 transition-colors"
								>
									<Square size={13} /> Stop
								</button>
								<button
									type="button"
									onclick={() => runAction(app, "restart")}
									disabled={isBusy}
									class="flex items-center gap-1.5 px-2.5 py-1.5 bg-surface hover:bg-surface-warm border border-line-soft rounded-lg text-xs font-medium text-ink-2 disabled:opacity-50 transition-colors"
								>
									<RotateCw size={13} /> Restart
								</button>
							{:else}
								<button
									type="button"
									onclick={() => runAction(app, "start")}
									disabled={isBusy}
									class="flex items-center gap-1.5 px-2.5 py-1.5 bg-brand hover:bg-brand-hover text-white rounded-lg text-xs font-medium disabled:opacity-50 transition-colors"
								>
									<Play size={13} /> Start
								</button>
							{/if}
						{/if}
						{#if app.properties && app.properties.length > 0}
							<button
								type="button"
								onclick={() => openConfigure(app)}
								class="ml-auto flex items-center gap-1.5 px-2.5 py-1.5 bg-canvas hover:bg-brand-soft border border-line-soft rounded-lg text-xs font-medium text-brand transition-colors"
							>
								<Sliders size={13} /> Configure
								{#if app.properties.some((p) => p.overridden)}
									<span class="ml-1 inline-block w-1.5 h-1.5 rounded-full bg-accent" title="Has overrides"></span>
								{/if}
							</button>
						{/if}
						{#if isBusy}
							<Loader2 size={14} class="text-ink-faint animate-spin ml-auto" />
						{/if}
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

<!-- Configure drawer -->
{#if configureApp}
	{@const app = configureApp}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="fixed inset-0 bg-ink/40 backdrop-blur-sm z-40 flex items-stretch justify-end"
		onclick={closeConfigure}
	>
		<div
			class="bg-canvas border-l border-line w-full max-w-md h-full overflow-y-auto shadow-2xl"
			onclick={(e) => e.stopPropagation()}
		>
			<div class="px-5 py-4 border-b border-line-soft flex items-start justify-between sticky top-0 bg-canvas">
				<div>
					<p class="text-xs uppercase tracking-wider text-ink-faint font-medium">Configure</p>
					<h3 class="text-lg font-semibold text-ink mt-0.5">{app.name}</h3>
					<p class="text-[11px] text-ink-faint font-mono">{app.id}</p>
				</div>
				<button
					type="button"
					onclick={closeConfigure}
					class="p-1.5 rounded-lg text-ink-muted hover:text-ink hover:bg-surface transition-colors"
					aria-label="Close"
				>
					<X size={18} />
				</button>
			</div>

			<div class="p-5 space-y-5">
				{#if app.properties.length === 0}
					<p class="text-ink-muted text-sm italic">This app has no configurable properties.</p>
				{:else}
					{#each app.properties as prop (prop.key)}
						<div class="space-y-1.5">
							<div class="flex items-center justify-between gap-2">
								<label for="prop-{prop.key}" class="text-sm font-medium text-ink">
									{prop.display || prop.key}
								</label>
								{#if prop.overridden || pendingConfig[prop.key] !== undefined}
									<button
										type="button"
										onclick={() => resetProperty(prop)}
										class="text-[11px] text-ink-faint hover:text-brand inline-flex items-center gap-1 transition-colors"
									>
										<RotateCcw size={11} /> Reset
									</button>
								{/if}
							</div>

							{#if prop.type === "enum" && prop.options}
								<select
									id="prop-{prop.key}"
									value={pendingValue(prop)}
									onchange={(e) => {
										const v = (e.target as HTMLSelectElement).value;
										if (v === prop.value) {
											const fresh = { ...pendingConfig };
											delete fresh[prop.key];
											pendingConfig = fresh;
										} else {
											pendingConfig = { ...pendingConfig, [prop.key]: v };
										}
									}}
									class="w-full bg-canvas text-ink border border-line rounded-lg px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-brand/30 focus:border-brand"
								>
									{#each prop.options as opt}
										<option value={opt}>{opt}</option>
									{/each}
								</select>
							{:else}
								<input
									id="prop-{prop.key}"
									type="text"
									value={pendingValue(prop)}
									oninput={(e) => {
										const v = (e.target as HTMLInputElement).value;
										if (v === prop.value) {
											const fresh = { ...pendingConfig };
											delete fresh[prop.key];
											pendingConfig = fresh;
										} else {
											pendingConfig = { ...pendingConfig, [prop.key]: v };
										}
									}}
									class="w-full bg-canvas text-ink border border-line rounded-lg px-3 py-2 text-sm font-mono focus:outline-none focus:ring-2 focus:ring-brand/30 focus:border-brand"
								/>
							{/if}

							<p class="text-[11px] text-ink-faint">
								Default: <span class="font-mono">{prop.default || "—"}</span>
								{#if prop.overridden}
									&middot; <span class="text-accent-hover">user override active</span>
								{/if}
							</p>
						</div>
					{/each}
				{/if}
			</div>

			<div class="px-5 py-4 border-t border-line-soft flex items-center justify-between gap-2 sticky bottom-0 bg-canvas">
				<button
					type="button"
					onclick={clearAllOverrides}
					disabled={!app.properties.some((p) => p.overridden)}
					class="text-xs text-coral hover:underline disabled:opacity-40 disabled:no-underline"
				>
					Clear all overrides
				</button>
				<div class="flex items-center gap-2">
					<button
						type="button"
						onclick={closeConfigure}
						class="px-3 py-1.5 rounded-lg text-sm font-medium bg-surface hover:bg-surface-warm border border-line-soft text-ink"
					>
						Close
					</button>
					<button
						type="button"
						onclick={saveConfig}
						disabled={!hasPendingChanges() || configSaving}
						class="px-3 py-1.5 rounded-lg text-sm font-medium bg-brand hover:bg-brand-hover text-white disabled:opacity-40 flex items-center gap-1.5"
					>
						{#if configSaving}
							<Loader2 size={13} class="animate-spin" />
						{/if}
						Save
					</button>
				</div>
			</div>
		</div>
	</div>
{/if}

<!-- Toast -->
{#if toast}
	<div class="fixed bottom-5 right-5 z-50">
		<div class="px-4 py-2.5 rounded-lg shadow-lg border text-sm font-medium {toast.tone === 'err' ? 'bg-surface-accent text-coral border-coral/40' : 'bg-canvas text-ink border-line-soft'}">
			{toast.text}
		</div>
	</div>
{/if}
