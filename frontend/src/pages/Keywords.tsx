import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  CircleCheck,
  CircleSlash,
  Eraser,
  Eye,
  FileText,
  FlaskConical,
  Pencil,
  Plus,
  RotateCcw,
  Trash2,
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
import { ApiError, api, type Keyword, type Module } from "@/lib/api";

const MODULE_LABELS: Record<string, string> = { zen: "Zen", cline: "Cline", "*": "全部模块" };
const SOURCE_LABELS: Record<string, string> = {
  builtin: "内置提示词",
  override: "自定义覆盖",
  none: "未配置",
};

interface FormState {
  id?: number;
  module: string;
  keyword: string;
  note: string;
  enabled: boolean;
}

const emptyForm: FormState = { module: "zen", keyword: "", note: "", enabled: true };

export default function KeywordsPage() {
  const qc = useQueryClient();
  const [module, setModule] = useState<Module>("zen");
  const [filter, setFilter] = useState<"all" | Module>("all");
  const [editing, setEditing] = useState<FormState | null>(null);
  const [testOpen, setTestOpen] = useState(false);
  const [promptOpen, setPromptOpen] = useState(false);
  const [testText, setTestText] = useState(
    "You are Claude Code, a CLI tool for software engineering tasks.",
  );

  const q = useQuery({
    queryKey: ["keywords", filter],
    queryFn: () => api.keywords(filter === "all" ? undefined : filter),
  });
  const invalidate = () => qc.invalidateQueries({ queryKey: ["keywords"] });

  const save = useMutation({
    mutationFn: (f: FormState) => {
      const payload = { module: f.module, keyword: f.keyword, note: f.note, enabled: f.enabled };
      return f.id ? api.updateKeyword(f.id, payload) : api.createKeyword(payload);
    },
    onSuccess: () => {
      toast.success(editing?.id ? "关键词已更新" : "关键词已创建");
      setEditing(null);
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败"),
  });

  const remove = useMutation({
    mutationFn: api.deleteKeyword,
    onSuccess: () => {
      toast.success("关键词已删除");
      invalidate();
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "删除失败"),
  });

  const toggle = useMutation({
    mutationFn: (k: Keyword) => api.updateKeyword(k.id, { enabled: !k.enabled }),
    onSuccess: invalidate,
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "切换失败"),
  });

  const reorder = useMutation({
    mutationFn: api.reorderKeywords,
    onSuccess: invalidate,
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "排序失败"),
  });

  const resetHits = useMutation({
    mutationFn: api.resetKeywordHits,
    onSuccess: () => {
      toast.success("命中次数已清零");
      invalidate();
    },
  });

  const test = useMutation({
    mutationFn: () => api.testKeyword(module, testText),
  });

  const keywords = q.data?.keywords ?? [];

  function move(index: number, dir: -1 | 1) {
    const next = [...keywords];
    const target = index + dir;
    if (target < 0 || target >= next.length) return;
    [next[index], next[target]] = [next[target], next[index]];
    reorder.mutate(next.map((k) => k.id));
  }

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <div>
          <h2 className="text-lg font-semibold">系统提示词注入</h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            系统提示词中出现关键词（忽略大小写）时，整条系统提示词会被替换为该模块的原生提示词
          </p>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Select value={filter} onValueChange={(v) => setFilter(v as typeof filter)}>
            <SelectTrigger className="w-[132px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部关键词</SelectItem>
              <SelectItem value="zen">仅 Zen</SelectItem>
              <SelectItem value="cline">仅 Cline</SelectItem>
            </SelectContent>
          </Select>
          <Button variant="outline" onClick={() => setPromptOpen(true)}>
            <FileText /> 提示词
          </Button>
          <Button variant="outline" onClick={() => setTestOpen(true)}>
            <FlaskConical /> 测试
          </Button>
          <Button onClick={() => setEditing({ ...emptyForm })}>
            <Plus /> 新增关键词
          </Button>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>关键词列表</CardTitle>
          <CardDescription>
            多条命中时按顺序取第一条。按模块筛选时，「全部模块」的关键词也会一并列出。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {q.isLoading ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">加载中…</p>
          ) : keywords.length === 0 ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
              暂无关键词，命中检测不会触发注入。
            </p>
          ) : (
            <Table className="table-fixed">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[68px]">顺序</TableHead>
                  <TableHead className="w-[104px]">模块</TableHead>
                  <TableHead className="w-[26%]">关键词</TableHead>
                  <TableHead className="w-[22%]">备注</TableHead>
                  <TableHead className="w-[84px]">命中</TableHead>
                  <TableHead className="w-[92px]">状态</TableHead>
                  <TableHead className="w-[150px] text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {keywords.map((k, i) => (
                  <TableRow key={k.id}>
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
                          disabled={i === keywords.length - 1}
                          title="下移"
                        >
                          <ArrowDown className="h-3 w-3" />
                        </Button>
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={k.isGlobal ? "outline" : "muted"}>
                        {MODULE_LABELS[k.module] ?? k.module}
                      </Badge>
                    </TableCell>
                    <TableCell className="overflow-hidden">
                      <code
                        className="block truncate rounded bg-[var(--color-muted)] px-1.5 py-0.5 text-xs"
                        title={k.keyword}
                      >
                        {k.keyword}
                      </code>
                    </TableCell>
                    <TableCell className="overflow-hidden">
                      <span
                        className="block truncate text-xs text-[var(--color-muted-foreground)]"
                        title={k.note}
                      >
                        {k.note || "—"}
                      </span>
                    </TableCell>
                    <TableCell>
                      <span className="tabular-nums text-sm">{k.hits}</span>
                    </TableCell>
                    <TableCell>
                      {k.enabled ? (
                        <Badge variant="success">
                          <CircleCheck className="h-3 w-3" /> 启用
                        </Badge>
                      ) : (
                        <Badge variant="muted">
                          <CircleSlash className="h-3 w-3" /> 停用
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex shrink-0 justify-end gap-0.5">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => toggle.mutate(k)}
                          title={k.enabled ? "停用" : "启用"}
                        >
                          <RotateCcw className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7"
                          onClick={() => resetHits.mutate(k.id)}
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
                              id: k.id,
                              module: k.module,
                              keyword: k.keyword,
                              note: k.note,
                              enabled: k.enabled,
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
                            if (confirm(`确认删除关键词「${k.keyword}」？`)) remove.mutate(k.id);
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

      {/* 关键词编辑 */}
      <Dialog open={editing !== null} onOpenChange={(o) => !o && setEditing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editing?.id ? "编辑关键词" : "新增关键词"}</DialogTitle>
            <DialogDescription>
              匹配忽略大小写；命中后该模块的系统提示词会被整体替换为原生提示词。
            </DialogDescription>
          </DialogHeader>

          {editing && (
            <div className="grid gap-4">
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
                <Label htmlFor="kw">关键词</Label>
                <Input
                  id="kw"
                  value={editing.keyword}
                  placeholder="例如：Claude Code"
                  onChange={(e) => setEditing({ ...editing, keyword: e.target.value })}
                />
                <p className="text-xs text-[var(--color-muted-foreground)]">
                  命中任意子串即可（忽略大小写）。例如关键词 <code>Claude Code</code> 会匹配
                  <code>claude code</code>、<code>CLAUDE CODE</code>。
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="note">备注</Label>
                <Input
                  id="note"
                  value={editing.note}
                  placeholder="例如：第三方客户端特征"
                  onChange={(e) => setEditing({ ...editing, note: e.target.value })}
                />
              </div>

              <div className="flex items-center justify-between rounded-lg border border-[var(--color-border)] p-3">
                <div>
                  <div className="text-sm font-medium">启用</div>
                  <div className="text-xs text-[var(--color-muted-foreground)]">停用后不参与检测</div>
                </div>
                <Switch
                  checked={editing.enabled}
                  onCheckedChange={(v) => setEditing({ ...editing, enabled: v })}
                />
              </div>
            </div>
          )}

          <DialogFooter>
            <Button variant="outline" onClick={() => setEditing(null)}>
              取消
            </Button>
            <Button
              onClick={() => editing && save.mutate(editing)}
              disabled={!editing?.keyword.trim() || save.isPending}
            >
              {save.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 提示词查看 / 覆盖 */}
      <PromptDialog open={promptOpen} onOpenChange={setPromptOpen} module={module} setModule={setModule} />

      {/* 测试面板 */}
      <Dialog open={testOpen} onOpenChange={setTestOpen}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle>关键词检测测试</DialogTitle>
            <DialogDescription>
              对样例文本执行检测，不会写入数据库，也不影响线上请求。
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label>模块</Label>
              <Select value={module} onValueChange={(v) => setModule(v as Module)}>
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
              <Label htmlFor="sample">样例系统提示词</Label>
              <Textarea
                id="sample"
                className="min-h-[110px] font-mono text-xs"
                value={testText}
                onChange={(e) => setTestText(e.target.value)}
              />
            </div>

            <Button onClick={() => test.mutate()} disabled={test.isPending || !testText}>
              <FlaskConical /> {test.isPending ? "检测中…" : "执行检测"}
            </Button>

            {test.data && (
              <div className="grid gap-3">
                <div className="flex flex-wrap items-center gap-2 text-sm">
                  <span className="text-[var(--color-muted-foreground)]">结果：</span>
                  {test.data.hit ? (
                    <>
                      <Badge variant="warning">命中「{test.data.matchedKeyword}」</Badge>
                      <span className="text-[var(--color-muted-foreground)]">
                        将替换为 {SOURCE_LABELS[test.data.promptSource]}（{test.data.promptLength} 字节）
                      </span>
                    </>
                  ) : (
                    <Badge variant="muted">未命中，提示词保持原样</Badge>
                  )}
                </div>
                {test.data.hit && !test.data.hasPrompt && (
                  <p className="text-xs text-[var(--color-warning)]">
                    该模块暂无可用提示词（无内置且未设置覆盖），因此不会发生替换。
                  </p>
                )}
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

/** PromptDialog 查看当前生效的原生提示词，并支持自定义覆盖与恢复默认。 */
function PromptDialog({
  open,
  onOpenChange,
  module,
  setModule,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  module: Module;
  setModule: (m: Module) => void;
}) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["prompt", module], queryFn: () => api.prompt(module) });
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState(false);

  useEffect(() => {
    if (q.data && !editing) setDraft(q.data.override || q.data.effective);
  }, [q.data, editing]);

  const save = useMutation({
    mutationFn: (text: string) => api.savePrompt(module, text),
    onSuccess: () => {
      toast.success("提示词已保存");
      setEditing(false);
      qc.invalidateQueries({ queryKey: ["prompt"] });
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败"),
  });

  const reset = useMutation({
    mutationFn: () => api.savePrompt(module, ""),
    onSuccess: () => {
      toast.success("已恢复为内置提示词");
      setEditing(false);
      qc.invalidateQueries({ queryKey: ["prompt"] });
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "恢复失败"),
  });

  const data = q.data;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle>原生提示词</DialogTitle>
          <DialogDescription>
            命中关键词时，系统提示词会被替换为这里的内容。
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="flex flex-wrap items-center gap-3">
            <Select value={module} onValueChange={(v) => setModule(v as Module)}>
              <SelectTrigger className="w-[150px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="zen">Zen</SelectItem>
                <SelectItem value="cline">Cline</SelectItem>
              </SelectContent>
            </Select>
            {data && (
              <Badge variant={data.source === "override" ? "warning" : "muted"}>
                当前：{SOURCE_LABELS[data.source]}
              </Badge>
            )}
            {data && !data.hasBuiltin && (
              <span className="text-xs text-[var(--color-muted-foreground)]">
                该模块暂无内置提示词，需自定义覆盖后才生效
              </span>
            )}
            <div className="ml-auto flex gap-2">
              {data?.hasBuiltin && data.source === "override" && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => reset.mutate()}
                  disabled={reset.isPending}
                >
                  <RotateCcw /> 恢复默认
                </Button>
              )}
              <Button size="sm" variant="outline" onClick={() => setEditing((v) => !v)}>
                <Eye /> {editing ? "只读" : "编辑"}
              </Button>
            </div>
          </div>

          {editing ? (
            <>
              <Textarea
                className="min-h-[320px] font-mono text-xs"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
              />
              <p className="text-xs text-[var(--color-muted-foreground)]">
                保存后会作为该模块的自定义覆盖（存于 SQLite）；清空内容并保存等同于恢复默认。
              </p>
            </>
          ) : (
            <pre className="max-h-[320px] overflow-auto rounded-lg border border-[var(--color-border)] bg-[var(--color-muted)] p-3 text-xs whitespace-pre-wrap">
              {data?.effective || "（无）"}
            </pre>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            关闭
          </Button>
          {editing && (
            <Button onClick={() => save.mutate(draft)} disabled={save.isPending}>
              {save.isPending ? "保存中…" : "保存覆盖"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
