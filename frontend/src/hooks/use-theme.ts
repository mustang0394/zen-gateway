import { useCallback, useSyncExternalStore } from "react";

/** 主题模式：跟随系统 / 浅色 / 深色 */
export type Theme = "system" | "light" | "dark";

export type EffectiveTheme = "light" | "dark";

const STORAGE_KEY = "zen_admin_theme";

/** 解析出实际生效的深浅色 */
function resolve(mode: Theme): EffectiveTheme {
  if (mode === "light" || mode === "dark") return mode;
  if (typeof window === "undefined") return "light";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function readStored(): Theme {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw === "light" || raw === "dark" || raw === "system" ? raw : "system";
  } catch {
    return "system";
  }
}

// ---- 共享主题 store -------------------------------------------------------
// 用一个模块级 store 保存主题，所有组件订阅同一份状态，
// 避免各自持有独立的 useState 导致切换按钮与提示气泡不同步。

let mode: Theme = readStored();
const listeners = new Set<() => void>();

function applyToDOM() {
  const effective = resolve(mode);
  document.documentElement.classList.toggle("dark", effective === "dark");
}

function emit() {
  applyToDOM();
  for (const fn of listeners) fn();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** 跟随系统模式下，监听系统配色变化（仅需一个全局监听器） */
if (typeof window !== "undefined") {
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    if (mode === "system") emit();
  });
}

// ---- 对外 hook ------------------------------------------------------------

/** 返回当前主题模式与设置方法；所有调用方共享同一份状态。 */
export function useTheme() {
  const current = useSyncExternalStore(
    subscribe,
    () => mode,
    () => "system" as Theme,
  );
  // effective 只在 mode 或系统偏好变化时改变，emit() 时同步派生即可
  const effective = resolve(current);

  const setTheme = useCallback((next: Theme) => {
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* 忽略存储不可用 */
    }
    mode = next;
    emit();
  }, []);

  return { theme: current, effective, setTheme };
}
