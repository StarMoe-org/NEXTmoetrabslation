/**
 * Typed API client for the moesekai v2 console backend.
 * All console calls hit /api/* (JWT, no-store). Public files are at /files/*.
 */

// ---- Types (mirror the Go backend) ----

export interface FieldInfo {
  name: string;
  total: number;
  cnCount: number;
  humanCount: number;
  pinnedCount: number;
  llmCount: number;
  unknownCount: number;
}

export interface CategoryInfo {
  name: string;
  fields: FieldInfo[];
}

export interface TranslationEntry {
  key: string;
  text: string;
  source: string;
  ids?: string[];
  speakerName?: string;
}

export interface EventStorySummary {
  eventId: number;
  source: string;
  episodeCount: number;
  untranslatedCount: number;
  lastUpdated: number;
}

export interface EventStoryEpisode {
  scenarioId: string;
  title: string;
  titleSource?: string;
  talkData: Record<string, string>;
  talkSources?: Record<string, string>;
  talkOrder?: string[];
  speakerNames?: Record<string, string>;
}

export interface EventStoryDetail {
  meta: { source: string; version: string; last_updated: number };
  episodes: Record<string, EventStoryEpisode>;
}

export interface TranslateStatus {
  translator: { running: boolean; lastRun?: string; lastMode?: string; lastError?: string; lastNote?: string };
  clients?: number;
}

export interface LoginResponse {
  token: string;
  username: string;
  role: "admin" | "editor";
  expiresAt: number;
}

export interface User {
  id: number;
  username: string;
  role: "admin" | "editor";
  createdAt: number;
}

export interface UpstreamStatus {
  enabled: boolean;
  repo?: string;
  branch?: string;
  versionURL?: string;
  lastCheck?: string;
  lastDataVersion?: string;
  changeDetectedAt?: string;
  lastSync?: string;
  lastError?: string;
  rateLimitedUntil?: string;
  gitMirrorReady?: boolean;
}

export interface BackupStatus {
  running: boolean;
  s3Enabled: boolean;
  gitEnabled: boolean;
  lastBackup?: string;
  lastS3Backup?: string;
  lastGitBackup?: string;
  lastRestore?: string;
  lastError?: string;
  dailyHourUtc: number;
}

// ---- Auth token storage ----

const TOKEN_KEY = "moesekai-token";
const USER_KEY = "moesekai-user";
const ROLE_KEY = "moesekai-role";

export function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(TOKEN_KEY);
}
export function getUsername(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem(USER_KEY) || "";
}
export function getRole(): "admin" | "editor" | "" {
  if (typeof window === "undefined") return "";
  return (localStorage.getItem(ROLE_KEY) as "admin" | "editor") || "";
}
export function setSession(r: LoginResponse) {
  localStorage.setItem(TOKEN_KEY, r.token);
  localStorage.setItem(USER_KEY, r.username);
  localStorage.setItem(ROLE_KEY, r.role);
}
export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
  localStorage.removeItem(ROLE_KEY);
}

// ---- Fetch helper ----

const API_BASE = process.env.NEXT_PUBLIC_API_BASE || "/api";

async function apiFetch<T>(path: string, options?: RequestInit): Promise<T> {
  const token = getToken();
  const res = await fetch(`${API_BASE}${path}`, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...options?.headers,
    },
  });
  if (res.status === 401) {
    clearSession();
    if (typeof window !== "undefined") window.location.reload();
    throw new Error("未授权");
  }
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

// ---- Auth ----

export const login = (username: string, password: string) =>
  apiFetch<LoginResponse>("/auth/login", { method: "POST", body: JSON.stringify({ username, password }) });
export const fetchMe = () => apiFetch<{ username: string; role: "admin" | "editor" }>("/auth/me");

// First-run setup: when no users exist, the console registers the first admin.
export const getSetupStatus = () => apiFetch<{ needsSetup: boolean }>("/auth/setup-status");
export const setupAdmin = (username: string, password: string) =>
  apiFetch<LoginResponse>("/auth/setup", { method: "POST", body: JSON.stringify({ username, password }) });

// ---- Translations ----

export const getCategories = () => apiFetch<CategoryInfo[]>("/categories");
export const getEntries = (category: string, field: string, source?: string) => {
  const p = new URLSearchParams({ category, field });
  if (source) p.set("source", source);
  return apiFetch<TranslationEntry[]>(`/entries?${p}`);
};
export const updateEntry = (category: string, field: string, key: string, text: string, source: string) =>
  apiFetch<{ status: string }>("/entry", {
    method: "PUT",
    body: JSON.stringify({ category, field, key, text, source }),
  });

// ---- Event stories ----

export const getEventStories = () => apiFetch<EventStorySummary[]>("/event-stories");
export const getEventStory = (eventId: number) => apiFetch<EventStoryDetail>(`/event-story?eventId=${eventId}`);
export const updateEventStoryLine = (
  eventId: number, episodeNo: string, jpKey: string, cnText: string,
  source = "human", entryType: "talk" | "title" = "talk",
) =>
  apiFetch<{ status: string }>("/event-story/update", {
    method: "PUT",
    body: JSON.stringify({ eventId, episodeNo, jpKey, cnText, source, entryType }),
  });
export const promoteEventStoryHuman = (eventId: number) =>
  apiFetch<{ status: string }>("/event-story/promote-human", { method: "POST", body: JSON.stringify({ eventId }) });
export const retryEventStory = (eventId: number) =>
  apiFetch<Record<string, unknown>>("/event-story/retry", { method: "POST", body: JSON.stringify({ eventId }) });
export const reorderEventStory = (eventId: number) =>
  apiFetch<Record<string, unknown>>("/event-story/reorder", { method: "POST", body: JSON.stringify({ eventId }) });

// ---- Translation engine ----

export const getTranslateStatus = () => apiFetch<TranslateStatus>("/translate/status");
export const runCNSync = () => apiFetch<Record<string, unknown>>("/translate/cn-sync", { method: "POST" });
export const triggerAITranslate = (category: string, field: string, provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai", { method: "POST", body: JSON.stringify({ category, field, provider }) });
export const triggerAITranslateAll = (provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai-all", { method: "POST", body: JSON.stringify({ provider }) });
export const triggerAIStory = (eventId: number, provider: "gemini" | "openai") =>
  apiFetch<Record<string, unknown>>("/translate/ai-story", { method: "POST", body: JSON.stringify({ eventId, provider }) });

// ---- Admin ----

export const listUsers = () => apiFetch<User[]>("/admin/users");
export const createUser = (username: string, password: string, role: "admin" | "editor") =>
  apiFetch<User>("/admin/users", { method: "POST", body: JSON.stringify({ username, password, role }) });
export const updateUser = (username: string, patch: { password?: string; role?: "admin" | "editor" }) =>
  apiFetch<{ status: string }>("/admin/users", { method: "PUT", body: JSON.stringify({ username, ...patch }) });
export const deleteUser = (username: string) =>
  apiFetch<{ status: string }>(`/admin/users?username=${encodeURIComponent(username)}`, { method: "DELETE" });

export const getSettings = () => apiFetch<{ settings: Record<string, string>; hasMasterKey: boolean }>("/admin/settings");
export const updateSettings = (patch: Record<string, string>) =>
  apiFetch<{ status: string; applied: number }>("/admin/settings", { method: "PUT", body: JSON.stringify(patch) });

export const getUpstreamStatus = () => apiFetch<UpstreamStatus>("/admin/upstream");
export const checkUpstream = (force = false) =>
  apiFetch<UpstreamStatus>("/admin/upstream/check", { method: "POST", body: JSON.stringify({ force }) });

// Read-only upstream status available to any authenticated user (user settings).
export const getUpstreamStatusPublic = () => apiFetch<UpstreamStatus>("/upstream/status");

export const getBackupStatus = () => apiFetch<BackupStatus>("/backup/status");
export const pushBackup = () => apiFetch<{ status: string; results: Record<string, string> }>("/backup/push", { method: "POST" });
export const restoreBackup = (target: "s3" | "git") =>
  apiFetch<Record<string, unknown>>("/backup/restore", { method: "POST", body: JSON.stringify({ target }) });
