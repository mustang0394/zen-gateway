// 管理端 API 客户端。鉴权走 HttpOnly 会话 cookie（登录后由浏览器自动携带），
// 也支持在 localStorage 保存 token 以便无人值守场景直接调用。

export type Module = "zen" | "cline";

export interface KeyCooldown {
  model: string;
  until: string;
  remaining: string;
  reason: string;
}

export interface ApiKey {
  ID: number;
  Module: Module;
  Label: string;
  Note: string;
  APIKey: string;
  Proxy: string;
  SortOrder: number;
  Enabled: boolean;
  IsAnonymous: boolean;
  CreatedAt: string;
  UpdatedAt: string;
  cooldowns?: KeyCooldown[] | null;
}

export interface KeyList {
  module: Module;
  keys: ApiKey[];
  anonymousSupported: boolean;
  anonymousHint: string;
}

export interface VersionRecord {
  Name: string;
  Value: string;
  FetchedAt: string;
  OK: boolean;
  Error: string;
}

export interface VersionsResponse {
  current: Record<string, string>;
  records: VersionRecord[];
  names: string[];
  zenUses: string;
  clineUses: string[];
}

export interface CooldownView {
  ID: number;
  Module: Module;
  KeyID: number;
  Model: string;
  Until: string;
  Reason: string;
  Detail: string;
  CreatedAt: string;
  keyLabel: string;
  remaining: string;
}

export interface StatSummary {
  requests: number;
  errors: number;
  retries: number;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  totalTokens: number;
  avgTtftMs: number;
  ttftCount: number;
  cacheHitRate: number;
}

export interface StatRow {
  day: string;
  module: Module;
  keyId: number;
  keyLabel: string;
  model: string;
  requests: number;
  errors: number;
  retries: number;
  promptTokens: number;
  completionTokens: number;
  cachedTokens: number;
  totalTokens: number;
  avgTtftMs: number;
  ttftCount: number;
  cacheHitRate: number;
}

export interface StatsResponse {
  day: string;
  module: string;
  summary: StatSummary;
  breakdown: StatRow[];
  days: string[] | null;
}

export interface Keyword {
  id: number;
  module: "zen" | "cline" | "*";
  keyword: string;
  note: string;
  enabled: boolean;
  sortOrder: number;
  hits: number;
  createdAt: string;
  updatedAt: string;
  isGlobal: boolean;
}

export interface KeywordList {
  keywords: Keyword[];
  modules: { value: string; label: string }[];
  note: string;
}

export interface KeywordTestResult {
  input: string;
  hit: boolean;
  matchedKeyword?: string;
  ruleId?: number;
  ruleNote?: string;
  promptSource: "builtin" | "override" | "none";
  promptLength: number;
  hasPrompt: boolean;
}

export interface PromptInfo {
  module: string;
  effective: string;
  builtin: string;
  override: string;
  source: "builtin" | "override" | "none";
  hasBuiltin: boolean;
}

export interface Settings {
  accessTokenZen: string;
  accessTokenCline: string;
  retentionDays: number;
  zenUpstream: string;
  clineUpstream: string;
}

const TOKEN_STORAGE_KEY = "zen_admin_token";

export function saveToken(t: string) {
  localStorage.setItem(TOKEN_STORAGE_KEY, t);
}
export function loadToken(): string {
  return localStorage.getItem(TOKEN_STORAGE_KEY) ?? "";
}
export function clearToken() {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    ...(init?.body ? { "Content-Type": "application/json" } : {}),
    ...((init?.headers as Record<string, string>) ?? {}),
  };
  const token = loadToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;

  const res = await fetch(`/admin/api${path}`, { ...init, headers });
  if (res.status === 401) {
    clearToken();
    throw new ApiError(401, "未授权：请重新登录");
  }
  const text = await res.text();
  let payload: unknown = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { error: text };
    }
  }
  if (!res.ok) {
    const msg =
      (payload as { error?: string })?.error ?? `请求失败（HTTP ${res.status}）`;
    throw new ApiError(res.status, msg);
  }
  return payload as T;
}

export const api = {
  login: (token: string) =>
    request<{ ok: boolean }>("/login", {
      method: "POST",
      body: JSON.stringify({ token }),
    }),

  settings: () => request<Settings>("/settings"),
  saveSettings: (s: Partial<Settings>) =>
    request<{ ok: boolean }>("/settings", { method: "PUT", body: JSON.stringify(s) }),

  versions: () => request<VersionsResponse>("/versions"),
  refreshVersions: () =>
    request<{ results: VersionRecord[] }>("/versions/refresh", { method: "POST" }),

  keys: (m: Module) => request<KeyList>(`/${m}/keys`),
  createKey: (
    m: Module,
    body: {
      label?: string;
      note?: string;
      apiKey: string;
      proxy?: string;
      enabled?: boolean;
      isAnonymous?: boolean;
    },
  ) =>
    request<{ key: ApiKey }>(`/${m}/keys`, { method: "POST", body: JSON.stringify(body) }),
  updateKey: (
    m: Module,
    id: number,
    body: {
      label?: string;
      note?: string;
      apiKey?: string;
      proxy?: string;
      enabled?: boolean;
      isAnonymous?: boolean;
    },
  ) =>
    request<{ key: ApiKey }>(`/${m}/keys/${id}`, { method: "PUT", body: JSON.stringify(body) }),
  deleteKey: (m: Module, id: number) =>
    request<{ ok: boolean }>(`/${m}/keys/${id}`, { method: "DELETE" }),
  reorderKeys: (m: Module, ids: number[]) =>
    request<{ ok: boolean }>(`/${m}/keys/reorder`, { method: "POST", body: JSON.stringify({ ids }) }),
  probeKey: (m: Module, id: number) =>
    request<{ ok: boolean; message: string }>(`/${m}/keys/${id}/probe?id=${id}`, { method: "POST" }),

  cooldowns: (m?: Module) =>
    request<{ cooldowns: CooldownView[] }>(`/cooldowns${m ? `?module=${m}` : ""}`),
  addCooldown: (body: { module: Module; keyId: number; model: string; minutes: number }) =>
    request<{ ok: boolean }>("/cooldowns", { method: "POST", body: JSON.stringify(body) }),
  releaseCooldown: (id: number) =>
    request<{ ok: boolean }>(`/cooldowns/${id}`, { method: "DELETE" }),

  keywords: (m?: Module | "*") => request<KeywordList>(`/keywords${m ? `?module=${m}` : ""}`),
  createKeyword: (body: { module: string; keyword: string; note?: string; enabled?: boolean }) =>
    request<{ keyword: Keyword }>("/keywords", { method: "POST", body: JSON.stringify(body) }),
  updateKeyword: (
    id: number,
    body: { module?: string; keyword?: string; note?: string; enabled?: boolean },
  ) => request<{ keyword: Keyword }>(`/keywords/${id}`, { method: "PUT", body: JSON.stringify(body) }),
  deleteKeyword: (id: number) => request<{ ok: boolean }>(`/keywords/${id}`, { method: "DELETE" }),
  reorderKeywords: (ids: number[]) =>
    request<{ ok: boolean }>("/keywords/reorder", { method: "POST", body: JSON.stringify({ ids }) }),
  resetKeywordHits: (id: number) =>
    request<{ ok: boolean }>(`/keywords/${id}/reset-hits`, { method: "POST" }),
  testKeyword: (module: Module, text: string) =>
    request<KeywordTestResult>("/keywords/test", {
      method: "POST",
      body: JSON.stringify({ module, text }),
    }),

  prompt: (m: Module) => request<PromptInfo>(`/prompts?module=${m}`),
  savePrompt: (m: Module, text: string) =>
    request<{ ok: boolean; source: string }>("/prompts", {
      method: "PUT",
      body: JSON.stringify({ module: m, text }),
    }),

  stats: (day?: string, m?: Module) => {
    const q = new URLSearchParams();
    if (day) q.set("day", day);
    if (m) q.set("module", m);
    const qs = q.toString();
    return request<StatsResponse>(`/stats${qs ? `?${qs}` : ""}`);
  },
};
