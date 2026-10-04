import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  Beaker,
  Eraser,
  Pencil,
  Plus,
  RotateCcw,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, api, type RewriteRule, type RewriteScope } from "@/lib/api";

const MODULE_LABELS: Record<string, string> = { zen: "Zen", cline: "Cline", "*": "全部模块" };
const SCOPE_LABELS: Record<RewriteScope, string> = {
  system: "仅系统提示词",
  system_first_user: "系统 + 首条用户消息",
  messages: "全部消息",
};

interface FormState {
  id?: number;
  module: string;
  name: string;
  match: string;
  replace: string;
  isRegex: boolean;
  caseSensitive: boolean;
  scope: RewriteScope;
  includeTools: boolean;
  enabled: boolean;
}

const emptyForm: FormState = {
  module: "zen",
  name: "",
  match: "",
  replace: "",
  isRegex: false,
  // 默认区分大小写：忽略大小写会扩大匹配面、提高误伤概率
  caseSensitive: true,
  scope: "system_first_user",
  includeTools: false,
  enabled: true,
};

export default function RewritesPage() {
  const qc = useQueryClient();
  const [filter, setFilter] = useState<"all" | "zen" | "cline">("all");
  const [editing, setEditing] = useState<FormState | null>(null);
  const [testOpen, setTestOpen] = useState(false);
  const [testModule, setTestModule] = useState<"zen" | "cline">("zen");
  const [testText, setTestText] = useState("You are OpenClaw, a coding agent. OpenClaw version v2.");

  const q = useQuery({
    queryKey: ["rewrites", filter],
    queryFn: () => api.rewrites(filter === "all" ? undefined : filter),
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["rewrites"] });

  const save = useMutation({
    mutationFn: (f: FormState) => {
      const payload = {
        module: f.module,
        name: f.name,
        match: f.match,
        replace: f.replace,
        isRegex: f.isRegex,
        caseSensitive: f.caseSensitive,
        scope: f.scope,
        includeTools: f.includeTools,
        enabled: f.enabled,
      };
      return f.id ? api.updateRewrite(f.id, payload) : api.createRewrite(payload);
    },
    onSuccess: () => {
      toast.success(editing?.id ? "规则已更新" : "规则已创建");
      setEditing(null);
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败"),
  });

  const remove = useMutation({
    mutationFn: api.deleteRewrite,
    onSuccess: () => {
      toast.success("规则已删除");
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "删除失败"),
  });

  const toggle = useMutation({
    mutationFn: (r: RewriteRule) => api.updateRewrite(r.ID, { enabled: !r.Enabled }),
    onSuccess: invalidate,
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "切换失败"),
  });

  const reorder = useMutation({
    mutationFn: api.reorderRewrites,
    onSuccess: invalidate,
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "排序失败"),
  });

  const resetHits = useMutation({
    mutationFn: api.resetRewriteHits,
    onSuccess: () => {
      toast.success("命中次数已清零");
      invalidate();
    },
  });

  const preview = useMutation({
    mutationFn: () => api.previewRewrite(testModule, testText),
  });

  const rules = q.data?.rules ?? [];

  function move(index: number, dir: -1 | 1) {
    const next = [...rules];
    const target = index + dir;
    if (target < 0 || target >= next.length) return;
    [next[index], next[target]] = [next[target], next[index]];
    reorder.mutate(next.map((r) => r.ID));
  }

  const previewDiff = useMemo(() => {
    const data = preview.data;
    if (!data) return null;
    return { ...data };
  }, [preview.data]);

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <div>
          <h2 className="text-lg font-semibold">提示词改写</h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            按规则替换发往上游的系统提示词关键词（例如把 <code>OpenClaw</code> 换成 <code>OpenCode</code>）
          </p>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Select value={filter} onValueChange={(v) => setFilter(v as typeof filter)}>
            <SelectTrigger className="w-[130px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部规则</SelectItem>
              <SelectItem value="zen">影响 Zen 的</SelectItem>
              <SelectItem value="cline">影响 Cline 的</SelectItem>
            </SelectContent>
          </Select>
          <Button variant="outline" onClick={() => setTestOpen(true)}>
            <Beaker /> 测试
          </Button>
          <Button onClick={() => setEditing({ ...emptyForm })}>
            <Plus /> 新增规则
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>规则列表</CardTitle>
          <CardDescription>
            规则按顺序依次执行（可链式生效）；按模块筛选时，对两者都生效的全局规则也会一并列出。
            改写会改变发往上游的提示词前缀，
            <span className="text-[var(--color-warning)]">可能降低上游缓存命中率</span>。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {q.isLoading ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">加载中…</p>
          ) : rules.length === 0 ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
              暂无规则。点击右上角「新增规则」创建。
            </p>
          ) : (
            <Table className="table-fixed">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[68px]">顺序</TableHead>
                  <TableHead className="w-[84px]">模块</TableHead>
                  <TableHead className="w-[18%]">匹配 → 替换</TableHead>
                  <TableHead className="w-[22%]">作用域 / 模式</TableHead>
                  <TableHead className="w-[90px]">命中</TableHead>
                  <TableHead className="w-[96px]">状态</TableHead>
                  <TableHead className="w-[158px] text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rules.map((r, i) => (
                  <TableRow key={r.ID}>
                    <TableCell>
                      <div className="flex items-center gap-0.5">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => move(i, -1)}
                          disabled={i === 0}
                          title="上移"
                        >
                          <ArrowUp className="h-3 w-3" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => move(i, 1)}
                          disabled={i === rules.length - 1}
                          title="下移"
                        >
                          <ArrowDown className="h-3 w-3" />
                        </Button>
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={r.Module === "*" ? "outline" : "muted"}>
                        {MODULE_LABELS[r.Module] ?? r.Module}
                      </Badge>
                    </TableCell>
                    <TableCell className="overflow-hidden">
                      <div className="truncate text-xs" title={`${r.Match} → ${r.Replace}`}>
                        <code className="rounded bg-[var(--color-muted)] px-1 py-0.5">{r.Match}</code>
                        <span className="mx-1 text-[var(--color-muted-foreground)]">→</span>
                        <code className="rounded bg-[var(--color-muted)] px-1 py-0.5">{r.Replace}</code>
                      </div>
                      {r.Name && (
                        <div className="mt-0.5 truncate text-xs text-[var(--color-muted-foreground)]">
                          {r.Name}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="overflow-hidden">
                      <div className="truncate text-xs" title={SCOPE_LABELS[r.Scope]}>
                        {SCOPE_LABELS[r.Scope] ?? r.Scope}
                      </div>
                      <div className="mt-0.5 flex flex-wrap gap-1">
                        {r.IsRegex && <Badge variant="outline">正则</Badge>}
                        {r.CaseSensitive && <Badge variant="outline">区分大小写</Badge>}
                        {r.IncludeTools && <Badge variant="outline">含工具描述</Badge>}
                      </div>
                      {r.compileError && (
                        <div
                          className="mt-1 flex items-center gap-1 truncate text-xs text-[var(--color-destructive)]"
                          title={r.compileError}
                        >
                          <TriangleAlert className="h-3 w-3 shrink-0" /> 规则无效，已跳过
                        </div>
                      )}
                    </TableCell>
                    <TableCell>
                      <span className="tabular-nums text-sm">{r.Hits}</span>
                    </TableCell>
                    <TableCell>
                      {r.Enabled ? (
                        <Badge variant="success">启用</Badge>
                      ) : (
                        <Badge variant="muted">停用</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex shrink-0 justify-end gap-0.5">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => toggle.mutate(r)}
                          title={r.Enabled ? "停用" : "启用"}
                        >
                          <RotateCcw className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => resetHits.mutate(r.ID)}
                          title="清零命中次数"
                        >
                          <Eraser className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() =>
                            setEditing({
                              id: r.ID,
                              module: r.Module,
                              name: r.Name,
                              match: r.Match,
                              replace: r.Replace,
                              isRegex: r.IsRegex,
                              caseSensitive: r.CaseSensitive,
                              scope: r.Scope,
                              includeTools: r.IncludeTools,
                              enabled: r.Enabled,
                            })
                          }
                          title="编辑"
                        >
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => {
                            if (confirm(`确认删除规则「${r.Name || r.Match}」？`)) remove.mutate(r.ID);
                          }}
                          title="删除"
                        >
                          <Trash2 className="h-3.5 w-3.5 text-[var(--color-destructive)]" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* 规则编辑对话框 */}
      <Dialog open={editing !== null} onOpenChange={(o) => !o && setEditing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing?.id ? "编辑规则" : "新增规则"}</DialogTitle>
            <DialogDescription>
              保存时会校验正则与空匹配；无效规则不会被写入，也不会发往上游。
            </DialogDescription>
          </DialogHeader>

          {editing && (
            <div className="grid gap-4">
              <div className="grid grid-cols-2 gap-4">
                <div className="grid gap-2">
                  <Label>生效模块</Label>
                  <Select
                    value={editing.module}
                    onValueChange={(v) => setEditing({ ...editing, module: v })}
                  >
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="zen">Zen</SelectItem>
                      <SelectItem value="cline">Cline</SelectItem>
                      <SelectItem value="*">全部模块</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="grid gap-2">
                  <Label>备注名</Label>
                  <Input
                    value={editing.name}
                    placeholder="例如：品牌词"
                    onChange={(e) => setEditing({ ...editing, name: e.target.value })}
                  />
                </div>
              </div>

              <div className="grid grid-cols-2 gap-4">
                <div className="grid gap-2">
                  <Label htmlFor="rw-match">匹配内容</Label>
                  <Input
                    id="rw-match"
                    value={editing.match}
                    placeholder="OpenClaw"
                    onChange={(e) => setEditing({ ...editing, match: e.target.value })}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="rw-replace">替换为</Label>
                  <Input
                    id="rw-replace"
                    value={editing.replace}
                    placeholder="OpenCode"
                    onChange={(e) => setEditing({ ...editing, replace: e.target.value })}
                  />
                </div>
              </div>

              <div className="grid gap-2">
                <Label>作用范围</Label>
                <Select
                  value={editing.scope}
                  onValueChange={(v) => setEditing({ ...editing, scope: v as RewriteScope })}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(q.data?.scopes ?? []).map((s) => (
                      <SelectItem key={s.value} value={s.value}>
                        {s.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  「系统 + 首条用户消息」可覆盖把系统提示词写进第一条用户消息的客户端。
                </p>
              </div>

              {[
                {
                  key: "isRegex" as const,
                  title: "正则匹配",
                  desc: "勾选后「匹配内容」按正则解释，替换内容可用 $1 引用捕获组",
                },
                {
                  key: "caseSensitive" as const,
                  title: "区分大小写",
                  desc: "默认开启（OpenClaw 不会匹配 openclaw）；关闭后匹配面更大，误伤概率更高",
                },
                {
                  key: "includeTools" as const,
                  title: "同时改写工具描述",
                  desc: "仅改 tools 的顶层 description，不影响参数结构",
                },
                {
                  key: "enabled" as const,
                  title: "启用",
                  desc: "停用后不参与改写",
                },
              ].map(({ key, title, desc }) => (
                <div
                  key={key}
                  className="flex items-center justify-between rounded-lg border border-[var(--color-border)] p-3"
                >
                  <div>
                    <div className="text-sm font-medium">{title}</div>
                    <div className="text-xs text-[var(--color-muted-foreground)]">{desc}</div>
                  </div>
                  <Switch
                    checked={editing[key]}
                    onCheckedChange={(v) => setEditing({ ...editing, [key]: v })}
                  />
                </div>
              ))}
            </div>
          )}

          <DialogFooter>
            <Button variant="outline" onClick={() => setEditing(null)}>
              取消
            </Button>
            <Button
              onClick={() => editing && save.mutate(editing)}
              disabled={!editing?.match.trim() || save.isPending}
            >
              {save.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 测试面板：试跑规则，不落库 */}
      <Dialog open={testOpen} onOpenChange={setTestOpen}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle>规则测试</DialogTitle>
            <DialogDescription>
              对样例文本试跑当前生效的规则，不会写入数据库，也不影响线上请求。
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label>模块</Label>
              <Select
                value={testModule}
                onValueChange={(v) => setTestModule(v as "zen" | "cline")}
              >
                <SelectTrigger className="w-[160px]">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="zen">Zen</SelectItem>
                  <SelectItem value="cline">Cline</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="grid gap-2">
              <Label htmlFor="rw-sample">样例文本</Label>
              <Textarea
                id="rw-sample"
                className="min-h-[110px] font-mono text-xs"
                value={testText}
                onChange={(e) => setTestText(e.target.value)}
              />
            </div>

            <Button onClick={() => preview.mutate()} disabled={preview.isPending || !testText}>
              <Beaker /> {preview.isPending ? "测试中…" : "执行测试"}
            </Button>

            {previewDiff && (
              <div className="grid gap-3">
                <div className="grid gap-2">
                  <Label>替换结果</Label>
                  <pre className="max-h-[160px] overflow-auto rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)] p-3 text-xs whitespace-pre-wrap break-all">
                    {previewDiff.result}
                  </pre>
                </div>
                <div className="flex flex-wrap items-center gap-2 text-sm">
                  <span className="text-[var(--color-muted-foreground)]">命中规则：</span>
                  {previewDiff.hits.length === 0 ? (
                    <Badge variant="muted">无命中</Badge>
                  ) : (
                    previewDiff.hits.map((h) => (
                      <Badge key={h.ruleId} variant="success">
                        {h.name || `#${h.ruleId}`} × {h.count}
                      </Badge>
                    ))
                  )}
                </div>
              </div>
            )}
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setTestOpen(false)}>
              关闭
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}