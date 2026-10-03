import { useState } from "react";
import { RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { ApiError, api, saveToken } from "@/lib/api";

export default function LoginPage({ onSuccess }: { onSuccess: () => void }) {
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!token.trim()) return;
    setBusy(true);
    try {
      await api.login(token.trim());
      saveToken(token.trim());
      toast.success("登录成功");
      onSuccess();
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "登录失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <div className="mb-2 flex h-11 w-11 items-center justify-center rounded-xl bg-[var(--color-success)]/15">
            <RefreshCw className="h-5 w-5 text-[var(--color-success)]" />
          </div>
          <CardTitle className="text-xl">zen-gateway 管理端</CardTitle>
          <CardDescription>请输入管理口令（ZEN_ADMIN_TOKEN）</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="token">管理口令</Label>
              <Input
                id="token"
                type="password"
                autoComplete="current-password"
                placeholder="••••••••"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
            </div>
            <Button type="submit" disabled={busy || !token.trim()}>
              {busy ? "验证中…" : "登录"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
