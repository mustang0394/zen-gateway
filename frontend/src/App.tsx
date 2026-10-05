import { useEffect, useState } from "react";
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import {
  Activity,
  KeyRound,
  RefreshCw,
  ShieldCheck,
  Server,
  Settings as Cog,
  Snowflake,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { ThemeToggleButton } from "@/components/theme-toggle";
import LoginPage from "@/pages/Login";
import KeysPage from "@/pages/Keys";
import VersionsPage from "@/pages/Versions";
import CooldownsPage from "@/pages/Cooldowns";
import KeywordsPage from "@/pages/Keywords";
import StatsPage from "@/pages/Stats";
import SettingsPage from "@/pages/Settings";
import { clearToken, loadToken } from "@/lib/api";

const navItems = [
  { to: "/", label: "总览", icon: Activity },
  { to: "/keys", label: "Key 管理", icon: KeyRound },
  { to: "/versions", label: "版本号", icon: Server },
  { to: "/cooldowns", label: "冷却池", icon: Snowflake },
  { to: "/keywords", label: "提示词注入", icon: ShieldCheck },
  { to: "/settings", label: "设置", icon: Cog },
];

export default function App() {
  const [authed, setAuthed] = useState(() => Boolean(loadToken()));

  useEffect(() => {
    const onStorage = () => setAuthed(Boolean(loadToken()));
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  if (!authed) {
    return <LoginPage onSuccess={() => setAuthed(true)} />;
  }

  return (
    <div className="min-h-screen bg-[var(--color-background)]">
      <header className="sticky top-0 z-40 border-b border-[var(--color-border)] bg-[var(--color-background)]/85 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-6 px-4">
          <div className="flex shrink-0 items-center gap-2 font-semibold">
            <RefreshCw className="h-4 w-4 text-[var(--color-success)]" />
            zen-gateway
          </div>
          <nav className="flex min-w-0 items-center gap-1 overflow-x-auto">
            {navItems.map(({ to, label, icon: Icon }) => (
              <NavLink
                key={to}
                to={to}
                end={to === "/"}
                className={({ isActive }) =>
                  [
                    "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm transition-colors",
                    isActive
                      ? "bg-[var(--color-secondary)] text-[var(--color-foreground)]"
                      : "text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]",
                  ].join(" ")
                }
              >
                <Icon className="h-3.5 w-3.5" />
                {label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-1">
            <ThemeToggleButton />
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                clearToken();
                setAuthed(false);
              }}
            >
              退出
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-7xl px-4 py-6">
        <Routes>
          <Route path="/" element={<StatsPage />} />
          <Route path="/keys" element={<KeysPage />} />
          <Route path="/versions" element={<VersionsPage />} />
          <Route path="/cooldowns" element={<CooldownsPage />} />
          <Route path="/keywords" element={<KeywordsPage />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}
