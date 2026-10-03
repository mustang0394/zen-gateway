import { Monitor, Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { type Theme, useTheme } from "@/hooks/use-theme";

const LABELS: Record<Theme, { label: string; icon: typeof Sun }> = {
  system: { label: "跟随系统", icon: Monitor },
  light: { label: "浅色", icon: Sun },
  dark: { label: "深色", icon: Moon },
};

/** 紧凑的图标按钮：在 跟随系统 → 浅色 → 深色 之间循环 */
export function ThemeToggleButton() {
  const { theme, effective, setTheme } = useTheme();
  const Icon = theme === "system" ? Monitor : effective === "dark" ? Moon : Sun;
  const order: Theme[] = ["system", "light", "dark"];

  return (
    <Button
      variant="ghost"
      size="icon"
      title={`当前：${LABELS[theme].label}（点击切换）`}
      aria-label="切换主题"
      onClick={() => {
        const next = order[(order.indexOf(theme) + 1) % order.length];
        setTheme(next);
      }}
    >
      <Icon />
    </Button>
  );
}

/** 下拉选择：明确展示三种模式 */
export function ThemeSelect() {
  const { theme, setTheme } = useTheme();
  return (
    <Select value={theme} onValueChange={(v) => setTheme(v as Theme)}>
      <SelectTrigger className="w-[130px]" aria-label="主题">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {(Object.keys(LABELS) as Theme[]).map((key) => {
          const { label, icon: Icon } = LABELS[key];
          return (
            <SelectItem key={key} value={key}>
              <span className="flex items-center gap-2">
                <Icon className="h-3.5 w-3.5" />
                {label}
              </span>
            </SelectItem>
          );
        })}
      </SelectContent>
    </Select>
  );
}
