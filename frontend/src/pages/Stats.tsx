import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { CircleAlert, Database, Gauge, Hash, Timer, TrendingUp } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { type Module, api } from "@/lib/api";
import { fmtCompact, fmtInt, fmtMs, fmtPct, todayStr } from "@/lib/utils";

export default function StatsPage() {
  const [module, setModule] = useState<Module | "all">("all");
  const [day, setDay] = useState(todayStr());

  const q = useQuery({
    queryKey: ["stats", module, day],
    queryFn: () => api.stats(day, module === "all" ? undefined : module),
  });

  const s = q.data?.summary;
  const days = q.data?.days ?? [];
  const rows = q.data?.breakdown ?? [];

  const cards = [
    { label: "总 Token", value: fmtCompact(s?.totalTokens), sub: `${fmtInt(s?.totalTokens)} tokens`, icon: Database },
    { label: "输入 / 输出", value: `${fmtCompact(s?.promptTokens)} / ${fmtCompact(s?.completionTokens)}`, sub: "prompt / completion", icon: TrendingUp },
    { label: "缓存命中", value: fmtCompact(s?.cachedTokens), sub: "cached tokens", icon: Hash },
    { label: "缓存命中率", value: fmtPct(s?.cacheHitRate), sub: "cached / prompt", icon: Gauge },
    {
      label: "平均首字",
      value: s?.ttftCount ? fmtMs(s.avgTtftMs) : "—",
      sub: s?.ttftCount ? `基于 ${fmtInt(s.ttftCount)} 次采样` : "暂无采样",
      icon: Timer,
    },
    { label: "请求数", value: fmtInt(s?.requests), sub: `${fmtInt(s?.errors)} 错误 · ${fmtInt(s?.retries)} 重试`, icon: CircleAlert },
  ];

  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Tabs value={module} onValueChange={(v) => setModule(v as Module | "all")}>
          <TabsList>
            <TabsTrigger value="all">全部</TabsTrigger>
            <TabsTrigger value="zen">Zen</TabsTrigger>
            <TabsTrigger value="cline">Cline</TabsTrigger>
          </TabsList>
        </Tabs>
        <div className="ml-auto flex shrink-0 items-center gap-2">
          <span className="shrink-0 text-sm text-[var(--color-muted-foreground)]">日期</span>
          <Select value={day} onValueChange={setDay}>
            {/* 宽度自适应内容并禁止换行：日期 + "（今天）" 需保持单行显示 */}
            <SelectTrigger className="w-auto min-w-[130px] whitespace-nowrap">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={todayStr()}>
                <span className="whitespace-nowrap">{todayStr()}（今天）</span>
              </SelectItem>
              {days
                .filter((d) => d !== todayStr())
                .map((d) => (
                  <SelectItem key={d} value={d}>
                    <span className="whitespace-nowrap">{d}</span>
                  </SelectItem>
                ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {cards.map(({ label, value, sub, icon: Icon }) => (
          <Card key={label}>
            <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
              <CardDescription>{label}</CardDescription>
              <Icon className="h-4 w-4 text-[var(--color-muted-foreground)]" />
            </CardHeader>
            <CardContent>
              <div className="text-2xl font-semibold tabular-nums">{value}</div>
              <p className="mt-1 text-xs text-[var(--color-muted-foreground)]">{sub}</p>
            </CardContent>
          </Card>
        ))}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>按 Key × 模型明细</CardTitle>
          <CardDescription>统计维度：日期 · 模块 · Key · 模型（保留 30 天）</CardDescription>
        </CardHeader>
        <CardContent>
          {q.isLoading ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">加载中…</p>
          ) : rows.length === 0 ? (
            <p className="py-8 text-center text-sm text-[var(--color-muted-foreground)]">
              该日暂无统计数据。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>模块</TableHead>
                  <TableHead>Key</TableHead>
                  <TableHead>模型</TableHead>
                  <TableHead className="text-right">请求</TableHead>
                  <TableHead className="text-right">输入</TableHead>
                  <TableHead className="text-right">输出</TableHead>
                  <TableHead className="text-right">缓存命中</TableHead>
                  <TableHead className="text-right">命中率</TableHead>
                  <TableHead className="text-right">平均 TTFT</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r, i) => (
                  <TableRow key={`${r.module}-${r.keyId}-${r.model}-${i}`}>
                    <TableCell>
                      <Badge variant="outline">{r.module}</Badge>
                    </TableCell>
                    <TableCell className="font-medium">{r.keyLabel || `#${r.keyId}`}</TableCell>
                    <TableCell>
                      <code className="text-xs">{r.model || "—"}</code>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtInt(r.requests)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtCompact(r.promptTokens)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtCompact(r.completionTokens)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtCompact(r.cachedTokens)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtPct(r.cacheHitRate)}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.ttftCount ? fmtMs(r.avgTtftMs) : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
