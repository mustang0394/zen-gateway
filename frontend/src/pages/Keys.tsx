import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, CircleCheck, CircleSlash, Pencil, Plus, Snowflake, Trash2, Zap } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { ApiError, api, type ApiKey, type Module } from "@/lib/api";

interface FormState {
  id?: number;
  label: string;
  note: string;
  apiKey: string;
  proxy: string;
  enabled: boolean;
  isAnonymous: boolean;
}

const emptyForm: FormState = {
  label: "",
  note: "",
  apiKey: "",
  proxy: "",
  enabled: true,
  isAnonymous: false,
};

export default function KeysPage() {
  const [module, setModule] = useState<Module>("zen");
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<FormState>(emptyForm);
  const qc = useQueryClient();

  const keysQuery = useQuery({ queryKey: ["keys", module], queryFn: () => api.keys(module) });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["keys"] });
    qc.invalidateQueries({ queryKey: ["cooldowns"] });
  };

  const save = useMutation({
    mutationFn: async (f: FormState) => {
      const payload = {
        label: f.label,
        note: f.note,
        apiKey: f.apiKey || (f.isAnonymous ? "public" : ""),
        proxy: f.proxy,
        enabled: f.enabled,
        isAnonymous: f.isAnonymous,
      };
      if (f.id) return api.updateKey(module, f.id, payload);
      return api.createKey(module, payload);
    },
    onSuccess: () => {
      toast.success(form.id ? "已更新" : "已新增");
      setOpen(false);
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败"),
  });

  const remove = useMutation({
    mutationFn: (id: number) => api.deleteKey(module, id),
    onSuccess: () => {
      toast.success("已删除（软删除，统计保留）");
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "删除失败"),
  });

  const probe = useMutation({
    mutationFn: (id: number) => api.probeKey(module, id),
    onSuccess: (r) => toast.success(r.message),
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "探测失败"),
  });

  const reorder = useMutation({
    mutationFn: (ids: number[]) => api.reorderKeys(module, ids),
    onSuccess: invalidate,
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "排序失败"),
  });

  const keys = keysQuery.data?.keys ?? [];

  function move(index: number, dir: -1 | 1) {
    const next = [...keys];
    const target = index + dir;
    if (target < 0 || target >= next.length) return;
    [next[index], next[target]] = [next[target], next[index]];
    reorder.mutate(next.map((k) => k.ID));
  }

  function openCreate() {
    setForm(emptyForm);
    setOpen(true);
  }

  function openEdit(k: ApiKey) {
    setForm({
      id: k.ID,
      label: k.Label,
      note: k.Note,
      apiKey: k.APIKey,
      proxy: k.Proxy,
      enabled: k.Enabled,
      isAnonymous: k.IsAnonymous,
    });
    setOpen(true);
  }

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Tabs value={module} onValueChange={(v) => setModule(v as Module)}>
          <TabsList>
            <TabsTrigger value="zen">Zen</TabsTrigger>
            <TabsTrigger value="cline">Cline</TabsTrigger>
          </TabsList>
        </Tabs>
        <div className="ml-auto">
          <Button onClick={openCreate}>
            <Plus /> 新增 Key
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{module === "zen" ? "Zen 上游 Key" : "Cline 上游 Key"}</CardTitle>
          <CardDescription>
            按顺序轮询使用（顺序优先以提升上游缓存命中率）；未指定代理则为直连。
            {keysQuery.data?.anonymousHint ? ` ${keysQuery.data.anonymousHint}` : ""}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {keysQuery.isLoading ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">加载中…</p>
          ) : keys.length === 0 ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
              暂无 Key，点击右上角新增。
            </p>
          ) : (
            // 始终使用 table-fixed：超长 key/代理只能在自身列内截断，不会撑宽整表。
            // 表格自身带 overflow-x-auto，窄屏下改为表格内部横向滚动，
            // 而不是把整页顶宽（此前 lg:table-fixed 在 <1024px 时会撑破页面）。
            <Table className="table-fixed">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[68px]">顺序</TableHead>
                  <TableHead className="w-[20%]">标签 / 备注</TableHead>
                  <TableHead className="w-[28%]">Key</TableHead>
                  <TableHead className="w-[20%]">代理</TableHead>
                  <TableHead className="w-[104px]">状态</TableHead>
                  {/* 三个 28px 图标按钮 + 间距实测约需 88px；列宽不足时按钮会向左溢出压住状态列，故留足 134px */}
                  <TableHead className="w-[134px] text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((k, i) => {
                  const cooling = k.cooldowns ?? [];
                  return (
                    <TableRow key={k.ID}>
                      <TableCell>
                        <div className="flex items-center gap-0.5">
                          <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => move(i, -1)} disabled={i === 0}>
                            <ArrowUp className="h-3 w-3" />
                          </Button>
                          <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => move(i, 1)} disabled={i === keys.length - 1}>
                            <ArrowDown className="h-3 w-3" />
                          </Button>
                        </div>
                      </TableCell>
                      <TableCell className="overflow-hidden">
                        <div className="flex min-w-0 items-center gap-2">
                          <span className="truncate font-medium" title={k.Label || `#${k.ID}`}>
                            {k.Label || `#${k.ID}`}
                          </span>
                          {k.IsAnonymous && (
                            <Badge variant="muted" className="shrink-0">
                              匿名
                            </Badge>
                          )}
                        </div>
                        {k.Note && (
                          <div className="mt-0.5 truncate text-xs text-[var(--color-muted-foreground)]" title={k.Note}>
                            {k.Note}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="overflow-hidden">
                        {/* 超长 key 必须单行截断，title 提供完整值 */}
                        <code
                          className="block truncate rounded bg-[var(--color-muted)] px-1.5 py-0.5 text-xs"
                          title={k.APIKey}
                        >
                          {k.APIKey}
                        </code>
                      </TableCell>
                      <TableCell className="overflow-hidden">
                        {k.Proxy ? (
                          <code className="block truncate text-xs" title={k.Proxy}>
                            {k.Proxy}
                          </code>
                        ) : (
                          <span className="text-xs text-[var(--color-muted-foreground)]">直连</span>
                        )}
                      </TableCell>
                      <TableCell className="overflow-hidden">
                        {!k.Enabled ? (
                          <Badge variant="muted">
                            <CircleSlash className="h-3 w-3" /> 停用
                          </Badge>
                        ) : cooling.length > 0 ? (
                          <div className="flex flex-col items-start gap-1">
                            {cooling.map((c) => (
                              <Badge
                                key={c.model}
                                variant="warning"
                                title={`${c.model} · ${c.reason}`}
                              >
                                <Snowflake className="h-3 w-3" /> {c.remaining}
                              </Badge>
                            ))}
                          </div>
                        ) : (
                          <Badge variant="success">
                            <CircleCheck className="h-3 w-3" /> 可用
                          </Badge>
                        )}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex shrink-0 justify-end gap-0.5">
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7"
                            onClick={() => probe.mutate(k.ID)}
                            title="校验代理配置"
                          >
                            <Zap className="h-3.5 w-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7"
                            onClick={() => openEdit(k)}
                            title="编辑"
                          >
                            <Pencil className="h-3.5 w-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7"
                            onClick={() => {
                              if (confirm(`确认删除「${k.Label || k.ID}」？将同时清除其冷却记录。`)) remove.mutate(k.ID);
                            }}
                            title="删除"
                          >
                            <Trash2 className="h-3.5 w-3.5 text-[var(--color-destructive)]" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{form.id ? "编辑 Key" : "新增 Key"}</DialogTitle>
            <DialogDescription>
              {module === "zen"
                ? "Zen 免费层支持匿名：开启「匿名模式」后上游 Key 将为 public（上游按 IP 限流）。"
                : "Cline 不支持匿名，必须提供有效的上游 Key。"}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="label">标签</Label>
              <Input
                id="label"
                placeholder="例如：主号"
                value={form.label}
                onChange={(e) => setForm({ ...form, label: e.target.value })}
              />
            </div>

            <div className="grid gap-2">
              <Label htmlFor="note">备注</Label>
              <Textarea
                id="note"
                maxLength={500}
                placeholder="例如：2026-10-01 注册，日本出口，限流 200/天"
                value={form.note}
                onChange={(e) => setForm({ ...form, note: e.target.value })}
              />
              <span className="text-xs text-[var(--color-muted-foreground)]">{form.note.length}/500</span>
            </div>

            <div className="grid gap-2">
              <Label htmlFor="apiKey">上游 Key</Label>
              <Input
                id="apiKey"
                placeholder={form.isAnonymous ? "public（匿名模式自动填充）" : "sk-..."}
                value={form.apiKey}
                disabled={form.isAnonymous}
                onChange={(e) => setForm({ ...form, apiKey: e.target.value })}
              />
            </div>

            <div className="grid gap-2">
              <Label htmlFor="proxy">代理（留空 = 直连）</Label>
              <Input
                id="proxy"
                placeholder="http://host:8080 或 socks5h://user:pass@host:1080"
                value={form.proxy}
                onChange={(e) => setForm({ ...form, proxy: e.target.value })}
              />
            </div>

            <div className="flex items-center justify-between rounded-lg border border-[var(--color-border)] p-3">
              <div>
                <div className="text-sm font-medium">匿名模式</div>
                <div className="text-xs text-[var(--color-muted-foreground)]">
                  仅 Zen 可用：上游按 IP 限流，无需 Key
                </div>
              </div>
              <Switch
                checked={form.isAnonymous}
                disabled={module !== "zen"}
                onCheckedChange={(v) =>
                  setForm({ ...form, isAnonymous: v, apiKey: v ? "public" : "" })
                }
              />
            </div>

            <div className="flex items-center justify-between rounded-lg border border-[var(--color-border)] p-3">
              <div>
                <div className="text-sm font-medium">启用</div>
                <div className="text-xs text-[var(--color-muted-foreground)]">停用后不参与轮询</div>
              </div>
              <Switch checked={form.enabled} onCheckedChange={(v) => setForm({ ...form, enabled: v })} />
            </div>
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              取消
            </Button>
            <Button onClick={() => save.mutate(form)} disabled={save.isPending}>
              {save.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
