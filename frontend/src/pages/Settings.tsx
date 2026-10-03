import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Save, ShieldAlert } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { ApiError, api } from "@/lib/api";

export default function SettingsPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["settings"], queryFn: api.settings });

  const [zenToken, setZenToken] = useState("");
  const [clineToken, setClineToken] = useState("");
  const [retention, setRetention] = useState("30");

  useEffect(() => {
    if (q.data) {
      setZenToken(q.data.accessTokenZen);
      setClineToken(q.data.accessTokenCline);
      setRetention(String(q.data.retentionDays));
    }
  }, [q.data]);

  const save = useMutation({
    mutationFn: () =>
      api.saveSettings({
        accessTokenZen: zenToken,
        accessTokenCline: clineToken,
        retentionDays: Number(retention) || 30,
      }),
    onSuccess: () => {
      toast.success("设置已保存");
      qc.invalidateQueries({ queryKey: ["settings"] });
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败"),
  });

  return (
    <div className="grid gap-4">
      <div>
        <h2 className="text-lg font-semibold">设置</h2>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          下游客户端使用这里的接入 Token 调用网关；网关再用 Key 池中的上游 Key 访问上游
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>下游接入 Token</CardTitle>
          <CardDescription>
            客户端需携带 <code>Authorization: Bearer &lt;token&gt;</code>
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="zenToken">Zen 模块（留空 = 允许匿名访问）</Label>
            <Input
              id="zenToken"
              value={zenToken}
              placeholder="留空表示无需鉴权即可调用 /zen/*"
              onChange={(e) => setZenToken(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="clineToken">Cline 模块（建议必填）</Label>
            <Input
              id="clineToken"
              value={clineToken}
              placeholder="例如：gw-cline-xxxx"
              onChange={(e) => setClineToken(e.target.value)}
            />
            {!clineToken.trim() && (
              <div className="flex items-center gap-2 text-xs text-[var(--color-warning)]">
                <ShieldAlert className="h-3.5 w-3.5" />
                未设置时 /cline/* 将接受任意 Token，建议尽快配置。
              </div>
            )}
          </div>
          <div className="grid gap-2 max-w-[220px]">
            <Label htmlFor="retention">统计保留天数</Label>
            <Input
              id="retention"
              type="number"
              min={1}
              value={retention}
              onChange={(e) => setRetention(e.target.value)}
            />
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>上游地址（只读）</CardTitle>
          <CardDescription>通过环境变量或启动参数配置</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 text-sm">
          <div className="flex justify-between gap-4">
            <span className="text-[var(--color-muted-foreground)]">Zen 上游</span>
            <code>{q.data?.zenUpstream ?? "—"}</code>
          </div>
          <div className="flex justify-between gap-4">
            <span className="text-[var(--color-muted-foreground)]">Cline 上游</span>
            <code>{q.data?.clineUpstream ?? "—"}</code>
          </div>
        </CardContent>
      </Card>

      <div className="flex justify-end">
        <Button onClick={() => save.mutate()} disabled={save.isPending}>
          <Save /> {save.isPending ? "保存中…" : "保存设置"}
        </Button>
      </div>
    </div>
  );
}
