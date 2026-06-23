/**
 * read-once — Pi Extension
 *
 * Prevents redundant file reads within a session. Tracks which files
 * have been read and blocks re-reads of unchanged files, saving context
 * tokens. The LLM sees a reason message and can move on.
 *
 * Intercepts both the `read` tool and bash commands that read files
 * (cat, head, tail, less, more, bat, etc.).
 *
 * Configuration via environment variables:
 *   READ_ONCE_MODE        = warn | deny | allow  (default: warn)
 *   READ_ONCE_TTL         = seconds              (default: 300)
 *   READ_ONCE_DISABLED    = 1 to disable
 *   READ_ONCE_EXCLUDE     = comma-separated glob patterns to skip
 *   READ_ONCE_MAX_BYTES   = skip files larger than this (default: 1MB)
 *   READ_ONCE_AUTO_ALLOW  = auto-allow re-read after N blocks (default: 2)
 *   READ_ONCE_DECAY       = seconds to consider attempts consecutive (default: 60)
 *   READ_ONCE_HASH        = 1 to validate by content hash (default: 1)
 *   READ_ONCE_DEBUG       = 1 for console.error diagnostics
 */

import { createHash } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import { basename, normalize, resolve } from "node:path";
import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

type CacheEntry = {
	mtime: number;
	ts: number;
	attempts: number;
	lastAttempt: number;
	hash?: string;
};

type StatsEntry = {
	event: string;
	path: string;
	tokensSaved: number;
	ts: number;
};

const MODE = (process.env.READ_ONCE_MODE || "warn").toLowerCase();
const TTL_S = parseInt(process.env.READ_ONCE_TTL || "300", 10);
const DEBUG = /^(1|true|yes)$/i.test(process.env.READ_ONCE_DEBUG || "");
const MAX_BYTES = parseInt(
	process.env.READ_ONCE_MAX_BYTES || String(1024 * 1024),
	10,
);
const AUTO_ALLOW = parseInt(process.env.READ_ONCE_AUTO_ALLOW || "2", 10);
const DECAY_S = parseInt(process.env.READ_ONCE_DECAY || "60", 10);
const HASH_MODE = /^(1|true|yes)$/i.test(process.env.READ_ONCE_HASH || "1");

const EXCLUDED = [
	".git/",
	"node_modules/",
	".DS_Store",
	"/proc/",
	"/sys/",
	"/dev/",
	".sock",
	".lock",
	".pid",
	...parseExcludePatterns(process.env.READ_ONCE_EXCLUDE || ""),
];

function parseExcludePatterns(raw: string): string[] {
	return raw
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
}

function shouldBypass(p: string): boolean {
	return EXCLUDED.some((ex) => p.includes(ex));
}

function fileHash(filePath: string): string {
	try {
		const data = readFileSync(filePath);
		return createHash("sha256").update(data).digest("hex").slice(0, 16);
	} catch {
		return "";
	}
}

function bashReadsFile(
	cmd: string,
): { path: string; offset?: number; limit?: number } | null {
	let c = cmd.replace(/\b\w+=\S+\s*/g, "").trim();
	c = c.replace(/^(?:sudo\s+|(?:time|nice|nohup|command|exec)\s+)+/, "").trim();
	c = c.split(/\s*[|><&]/, 1)[0].trim();

	const head = /^[a-z/._-]+/i.exec(c)?.[0] || "";
	const bin = head.includes("/") ? head.split("/").pop()! : head;
	const readBins = new Set([
		"cat",
		"head",
		"tail",
		"less",
		"more",
		"bat",
		"highlight",
		"nl",
	]);
	if (!readBins.has(bin)) return null;

	const args = c.slice(bin.length).trim();
	if (!args || args.startsWith("-")) {
		const parts = args.split(/\s+/);
		const fileArg = parts.find(
			(p) => p && !p.startsWith("-") && !p.includes("="),
		);
		if (!fileArg) return null;
	}

	let offset: number | undefined;
	let limit: number | undefined;
	const files: string[] = [];
	const tokens = args.split(/\s+/);

	for (let i = 0; i < tokens.length; i++) {
		const t = tokens[i];
		if (!t) continue;
		if (t === "-n" || t === "--lines" || t === "-") {
			const v = tokens[i + 1];
			if (/^\d+$/.test(v)) {
				limit = parseInt(v, 10);
				i++;
			}
			continue;
		}
		if (t.startsWith("-n") && /^\d+$/.test(t.slice(2))) {
			limit = parseInt(t.slice(2), 10);
			continue;
		}
		if (/^\+\d+$/.test(t)) {
			offset = parseInt(t.slice(1), 10);
			continue;
		}
		if (t.startsWith("-")) continue;
		files.push(t);
	}

	if (files.length !== 1) return null;
	const result: { path: string; offset?: number; limit?: number } = {
		path: files[0],
	};
	if (offset !== undefined) result.offset = offset;
	if (limit !== undefined) result.limit = limit;
	return result;
}

export default function (pi: ExtensionAPI) {
	if (/^(1|true|yes)$/i.test(process.env.READ_ONCE_DISABLED || "")) return;

	const cache = new Map<string, CacheEntry>();
	const sessionStart = Date.now();
	const stats: StatsEntry[] = [];

	function debug(msg: string) {
		if (DEBUG) console.error(`[read-once] ${msg}`);
	}

	// Adaptive TTL: grows with session duration (5min→15min over 20min)
	function adaptiveTTL(): number {
		const sessionAge = (Date.now() - sessionStart) / 1000;
		const multiplier = 1.0 + Math.min(sessionAge / 600, 2.0);
		return Math.min(Math.floor(TTL_S * multiplier), TTL_S * 3);
	}

	function isExpired(entry: CacheEntry, now: number): boolean {
		return now - entry.ts > adaptiveTTL() * 1000;
	}

	function checkFile(
		filePath: string,
		cwd: string,
		offset?: number,
		limit?: number,
	): { block: boolean; reason: string } | undefined {
		if (!filePath) return undefined;

		const absPath = filePath.startsWith("/")
			? filePath
			: resolve(cwd, filePath);
		const cleanPath = normalize(absPath);

		if (shouldBypass(cleanPath)) {
			debug(`bypass: ${cleanPath}`);
			return undefined;
		}

		const hasRange = offset !== undefined;
		const cacheKey = hasRange
			? `${cleanPath}:${offset}:${limit ?? 0}`
			: cleanPath;
		const now = Date.now();
		const ttl = adaptiveTTL();

		try {
			const st = statSync(cleanPath);
			if (st.isDirectory()) return undefined;
			if (MAX_BYTES > 0 && st.size > MAX_BYTES) {
				debug(`too large: ${cleanPath} (${st.size} bytes)`);
				return undefined;
			}
		} catch {
			return undefined;
		}

		const entry = cache.get(cacheKey);

		if (entry && !isExpired(entry, now)) {
			// Re-stat to detect mtime changes
			try {
				const st = statSync(cleanPath);
				if (st.mtimeMs !== entry.mtime) {
					debug(`changed: ${cleanPath} (mtime)`);
					const hash = HASH_MODE ? fileHash(cleanPath) : undefined;
					cache.set(cacheKey, {
						mtime: st.mtimeMs,
						ts: now,
						attempts: 0,
						lastAttempt: now,
						hash,
					});
					stats.push({
						event: "changed",
						path: cleanPath,
						tokensSaved: 0,
						ts: now,
					});
					return undefined;
				}
			} catch {
				cache.delete(cacheKey);
				return undefined;
			}

			// Hash validation: catches touch-only changes
			if (HASH_MODE && entry.hash) {
				const currentHash = fileHash(cleanPath);
				if (currentHash && currentHash !== entry.hash) {
					debug(`changed: ${cleanPath} (hash)`);
					cache.set(cacheKey, {
						mtime: entry.mtime,
						ts: now,
						attempts: 0,
						lastAttempt: now,
						hash: currentHash,
					});
					stats.push({
						event: "changed",
						path: cleanPath,
						tokensSaved: 0,
						ts: now,
					});
					return undefined;
				}
			}

			// Cache hit — track attempts for auto-allow
			const attempts =
				now - entry.lastAttempt <= DECAY_S * 1000 ? entry.attempts + 1 : 1;
			cache.set(cacheKey, { ...entry, ts: now, attempts, lastAttempt: now }); // sliding TTL

			if (AUTO_ALLOW > 0 && attempts >= AUTO_ALLOW) {
				debug(`auto-allow: ${cacheKey} (${attempts}/${AUTO_ALLOW})`);
				cache.set(cacheKey, {
					mtime: entry.mtime,
					ts: now,
					attempts: 0,
					lastAttempt: now,
					hash: entry.hash,
				});
				stats.push({
					event: "auto_allow",
					path: cleanPath,
					tokensSaved: 0,
					ts: now,
				});
				return undefined;
			}

			const minutesAgo = Math.floor((now - entry.ts) / 60000);
			const ttlMin = Math.floor(ttl / 60);
			const label = hasRange
				? `${basename(cleanPath)}:${offset}:${limit ?? 0}`
				: basename(cleanPath);
			let reason = `read-once: ${label} is already in context (read ${minutesAgo}m ago, unchanged). Re-read allowed after ${ttlMin}m.`;
			if (AUTO_ALLOW > 0) {
				reason += ` Attempt ${attempts}/${AUTO_ALLOW} before auto-allow.`;
			}

			if (MODE === "allow") {
				debug(`allow: ${cacheKey}`);
				cache.set(cacheKey, {
					mtime: entry.mtime,
					ts: now,
					attempts: 0,
					lastAttempt: now,
					hash: entry.hash,
				});
				return undefined;
			}

			debug(`blocked: ${cacheKey} (${MODE})`);
			// Estimate tokens saved (rough: 1 token per 4 chars, 170/100 factor)
			try {
				const size = statSync(cleanPath).size;
				const tokensSaved = Math.floor(((size / 4) * 170) / 100);
				stats.push({ event: "hit", path: cleanPath, tokensSaved, ts: now });
			} catch {
				/* ignore */
			}
			return { block: true, reason };
		}

		// Not cached or expired — record it
		try {
			const st = statSync(cleanPath);
			const hash = HASH_MODE ? fileHash(cleanPath) : undefined;
			cache.set(cacheKey, {
				mtime: st.mtimeMs,
				ts: now,
				attempts: 0,
				lastAttempt: now,
				hash,
			});
			debug(`cached: ${cacheKey}`);
			const event = entry ? "expired" : "miss";
			stats.push({ event, path: cleanPath, tokensSaved: 0, ts: now });
		} catch {
			// file gone, skip
		}

		return undefined;
	}

	// Intercept tool calls
	pi.on("tool_call", async (event, ctx) => {
		const cwd = ctx.cwd || process.cwd();

		if (event.toolName === "read") {
			const filePath = (event.input.path || event.input.file_path) as string;
			const offset = event.input.offset as number | undefined;
			const limit = event.input.limit as number | undefined;
			return checkFile(filePath, cwd, offset, limit);
		}

		if (event.toolName === "bash") {
			const command = event.input.command as string;
			if (!command) return undefined;
			const parsed = bashReadsFile(command);
			if (!parsed) return undefined;
			return checkFile(parsed.path, cwd, parsed.offset, parsed.limit);
		}

		return undefined;
	});

	// Register the clear-cache tool
	pi.registerTool({
		name: "readOnceClearCache",
		label: "Clear Read Cache",
		description:
			"Clear the read-once file cache, allowing re-reads of previously cached files. Use when you need to force a fresh read of a file that was blocked.",
		parameters: Type.Object({
			file_path: Type.Optional(
				Type.String({
					description: "Specific file to clear from cache. Omit to clear all.",
				}),
			),
		}),
		async execute(_toolCallId, params: any) {
			const filePath = params?.file_path || params?.path;
			if (filePath && typeof filePath === "string") {
				const cwd = process.cwd();
				const absPath = filePath.startsWith("/")
					? filePath
					: resolve(cwd, filePath);
				const cleanPath = normalize(absPath);
				let cleared = 0;
				for (const key of cache.keys()) {
					if (key === cleanPath || key.startsWith(cleanPath + ":")) {
						cache.delete(key);
						cleared++;
					}
				}
				return {
					content: [
						{
							type: "text",
							text:
								cleared > 0
									? `Cache cleared for ${cleanPath} (${cleared} entries). Next read will fetch fresh content.`
									: `${cleanPath} was not in cache.`,
						},
					],
				};
			}
			const count = cache.size;
			cache.clear();
			return {
				content: [
					{
						type: "text",
						text: `Cleared ${count} cached file(s). All files can be re-read now.`,
					},
				],
			};
		},
	});

	// Register the stats tool
	pi.registerTool({
		name: "readOnceStats",
		label: "Read-Once Stats",
		description:
			"Show read-once cache statistics for this session: hits, misses, tokens saved, and top re-read files.",
		parameters: Type.Object({}),
		async execute() {
			const hits = stats.filter((s) => s.event === "hit");
			const misses = stats.filter((s) => s.event === "miss");
			const changed = stats.filter((s) => s.event === "changed");
			const expired = stats.filter((s) => s.event === "expired");
			const autoAllows = stats.filter((s) => s.event === "auto_allow");
			const tokensSaved = hits.reduce((sum, s) => sum + s.tokensSaved, 0);
			const sessionAge = Math.floor((Date.now() - sessionStart) / 60000);

			// Top re-read files
			const fileCounts = new Map<string, number>();
			for (const h of hits) {
				const name = basename(h.path);
				fileCounts.set(name, (fileCounts.get(name) || 0) + 1);
			}
			const topFiles = [...fileCounts.entries()]
				.sort((a, b) => b[1] - a[1])
				.slice(0, 5)
				.map(([name, count]) => `  ${count}x  ${name}`)
				.join("\n");

			const lines = [
				`read-once - session stats`,
				``,
				`  Session duration:  ${sessionAge} min`,
				`  Cache hits:        ${hits.length} (blocked re-reads)`,
				`  First reads:       ${misses.length}`,
				`  Changed files:     ${changed.length}`,
				`  TTL expired:       ${expired.length}`,
				`  Auto-allow:        ${autoAllows.length}`,
				`  Cache size:        ${cache.size} entries`,
				`  Effective TTL:     ${Math.floor(adaptiveTTL() / 60)} min`,
				`  Tokens saved:      ~${tokensSaved.toLocaleString()}`,
			];
			if (tokensSaved > 0) {
				lines.push(
					`  Est. cost saved:   $${((tokensSaved * 3) / 1_000_000).toFixed(4)} (Sonnet) / $${((tokensSaved * 15) / 1_000_000).toFixed(4)} (Opus)`,
				);
			}
			if (topFiles) {
				lines.push(``, `  Top re-read files:`, topFiles);
			}

			return {
				content: [{ type: "text", text: lines.join("\n") }],
			};
		},
	});

	debug(
		`loaded (mode=${MODE}, ttl=${TTL_S}s, hash=${HASH_MODE ? "on" : "off"}, auto_allow=${AUTO_ALLOW})`,
	);
}
