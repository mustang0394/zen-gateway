import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Snowflake, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, api, type Module } from "@/lib/api";
import { fmtTime } from "@/lib/utils";

export default function CooldownsPage() {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [module, setModule] = useState<Module>("zen");
  const [keyId, setKeyId] = useState("");
  const [model, setModel] = useState("");
  const [minutes, setMinutes] = useState("60");

  const q = useQuery({ queryKey: ["cooldowns"], queryFn: () => api.cooldowns() });
  const keysQ = useQuery({ queryKey: ["keys", module], queryFn: () => api.keys(module) });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["cooldowns"] });
    qc.invalidateQueries({ queryKey: ["keys"] });
  };

  const release = useMutation({
    mutationFn: api.releaseCooldown,
    onSuccess: () => {
      toast.success("冷却已解除");
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "解除失败"),
  });

  const add = useMutation({
    mutationFn: () =>
      api.addCooldown({
        module,
        keyId: Number(keyId),
        model: model.trim(),
        minutes: Number(minutes) || 60,
      }),
    onSuccess: () => {
      toast.success("已加入冷却池");
      setOpen(false);
      setModel("");
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "添加失败"),
  });

  const items = q.data?.cooldowns ?? [];

  return (
    <div className="grid gap-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">冷却池</h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            维度为「Key + 模型」：某 Key 的某模型冷却期间，该 Key 的其他模型仍可正常使用
          </p>
        </div>
        <Button onClick={() => setOpen(true)}>
          <Plus /> 手动添加
        </Button>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>当前冷却</CardTitle>
          <CardDescription>Zen 在 429 时冷却到次日 0 点；Cline 按上游错误信息中的时长冷却</CardDescription>
        </CardHeader>
        <CardContent>
          {q.isLoading ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">加载中…</p>
          ) : items.length === 0 ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
              冷却池为空，所有 Key 均可用。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>模块</TableHead>
                  <TableHead>Key</TableHead>
                  <TableHead>模型</TableHead>
                  <TableHead>剩余</TableHead>
                  <TableHead>原因</TableHead>
                  <TableHead>到期时间</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((c) => (
                  <TableRow key={c.ID}>
                    <TableCell>
                      <Badge variant="outline">{c.Module}</Badge>
                    </TableCell>
                    <TableCell className="font-medium">{c.keyLabel || `#${c.KeyID}`}</TableCell>
                    <TableCell>
                      <code className="text-xs">{c.Model}</code>
                    </TableCell>
                    <TableCell>
                      <Badge variant="warning">
                        <Snowflake className="h-3 w-3" /> {c.remaining}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs text-[var(--color-muted-foreground)]">
                      {c.Reason}
                    </TableCell>
                    <TableCell className="text-xs text-[var(--color-muted-foreground)]">
                      {fmtTime(c.Until)}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" onClick={() => release.mutate(c.ID)}>
                        <Trash2 className="h-3.5 w-3.5 text-[var(--color-destructive)]" /> 解除
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>手动添加冷却</DialogTitle>
            <DialogDescription>让指定 Key 的指定模型在一段时间内不参与请求</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label>模块</Label>
              <Select value={module} onValueChange={(v) => { setModule(v as Module); setKeyId(""); }}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="zen">Zen</SelectItem>
                  <SelectItem value="cline">Cline</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label>Key</Label>
              <Select value={keyId} onValueChange={setKeyId}>
                <SelectTrigger>
                  <SelectValue placeholder="选择 Key" />
                </SelectTrigger>
                <SelectContent>
                  {(keysQ.data?.keys ?? []).map((k) => (
                    <SelectItem key={k.ID} value={String(k.ID)}>
                      {k.Label || `#${k.ID}`} · {k.APIKey}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="model">模型</Label>
              <Input
                id="model"
                placeholder="例如：z-ai/glm-5.3-flash"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="minutes">冷却时长（分钟）</Label>
              <Input
                id="minutes"
                type="number"
                min={1}
                value={minutes}
                onChange={(e) => setMinutes(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button onClick={() => add.mutate()} disabled={!keyId || !model.trim() || add.isPending}>
              添加
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
