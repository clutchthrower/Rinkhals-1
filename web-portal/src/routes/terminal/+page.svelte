<script lang="ts">
	import { onMount } from 'svelte';
	import { Terminal } from '@xterm/xterm';
	import { FitAddon } from '@xterm/addon-fit';
	import { Terminal as TerminalIcon } from 'lucide-svelte';
	import '@xterm/xterm/css/xterm.css';

	let terminalContainer: HTMLElement;
	let terminal: Terminal;
	let fitAddon: FitAddon;

	onMount(() => {
		terminal = new Terminal({
			cursorBlink: true,
			theme: {
				background: '#1a1a1a',
				foreground: '#f5f5f5',
				cursor: '#ff9a00',
				selectionBackground: '#005aff55',
			},
			fontFamily: '"Fira Code", "JetBrains Mono", monospace',
			fontSize: 13,
		});

		fitAddon = new FitAddon();
		terminal.loadAddon(fitAddon);
		terminal.open(terminalContainer);
		fitAddon.fit();

		const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
		const wsHost = import.meta.env.DEV ? 'localhost:8080' : window.location.host;
		const ws = new WebSocket(`${protocol}//${wsHost}/api/terminal`);
		ws.binaryType = 'arraybuffer';

		ws.onopen = () => {
			const dims = { cols: terminal.cols, rows: terminal.rows };
			ws.send(JSON.stringify(dims));
			terminal.focus();
		};

		ws.onmessage = (event) => {
			if (event.data instanceof ArrayBuffer) {
				terminal.write(new Uint8Array(event.data));
			} else {
				terminal.write(event.data);
			}
		};

		terminal.onData((data) => {
			if (ws.readyState === WebSocket.OPEN) {
				ws.send(data);
			}
		});

		terminal.onResize((size) => {
			if (ws.readyState === WebSocket.OPEN) {
				ws.send(JSON.stringify({ cols: size.cols, rows: size.rows }));
			}
		});

		const resizeObserver = new ResizeObserver(() => {
			fitAddon.fit();
		});
		resizeObserver.observe(terminalContainer);

		return () => {
			resizeObserver.disconnect();
			terminal.dispose();
			ws.close();
		};
	});
</script>

<svelte:head>
	<title>Terminal - Rinkhals</title>
</svelte:head>

<div class="h-full flex flex-col space-y-4 max-w-7xl mx-auto w-full">
	<header class="flex items-end justify-between pb-4 border-b border-line-soft">
		<div>
			<p class="text-xs uppercase tracking-wider text-ink-faint font-medium">Shell</p>
			<h2 class="text-3xl font-semibold text-ink mt-1 tracking-tight flex items-center gap-2">
				<TerminalIcon size={26} class="text-brand" />
				Console
			</h2>
		</div>
		<span class="text-xs text-ink-faint font-mono">/bin/sh</span>
	</header>

	<div class="bg-canvas rounded-xl border border-line-soft flex-1 relative overflow-hidden">
		<div class="px-4 py-2 border-b border-line-soft bg-surface flex items-center gap-2">
			<span class="w-2.5 h-2.5 rounded-full bg-coral"></span>
			<span class="w-2.5 h-2.5 rounded-full bg-accent"></span>
			<span class="w-2.5 h-2.5 rounded-full bg-brand"></span>
			<span class="ml-3 text-xs text-ink-faint font-mono">interactive shell</span>
		</div>
		<div class="absolute inset-0 top-9 p-3 bg-[#1a1a1a]">
			<div bind:this={terminalContainer} class="absolute inset-0 p-3"></div>
		</div>
	</div>
</div>
