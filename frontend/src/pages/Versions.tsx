import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleCheck, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, api } from "@/lib/api";
import { fmtTime } from "@/lib/utils";

const LABELS: Record<string, { title: string; usage: string }> = {
  "zen": { title: "OpenCode Zen", usage: "User-Agent: opencode/<版本>" },
  "cline.cli": {
    title: "Cline CLI",
    usage: "X-CLIENT-VERSION / X-PLATFORM-VERSION / User-Agent: Cline/<版本>",
  },
  "cline.sdk": { title: "Cline SDK", usage: "X-CORE-VERSION: <版本>" },
};

export default function VersionsPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["versions"], queryFn: api.versions });

  const refresh = useMutation({
    mutationFn: api.refreshVersions,
    onSuccess: (r) => {
      const failed = r.results.filter((x) => !x.OK);
      if (failed.length) {
        toast.warning(`部分目标刷新失败：${failed.map((f) => f.Name).join(", ")}`);
      } else {
        toast.success("版本号已刷新");
      }
      qc.invalidateQueries({ queryKey: ["versions"] });
    },
    onError: (e) => toast.error(e instanceof ApiError ? e.message : "刷新失败"),
  });

  const records = q.data?.records ?? [];

  return (
    <div className="grid gap-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">上游版本号</h2>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            每日自动从 GitHub 抓取一次，也可手动刷新（用于构造上游所需的客户端请求头）
          </p>
        </div>
        <Button onClick={() => refresh.mutate()} disabled={refresh.isPending}>
          <RefreshCw className={refresh.isPending ? "animate-spin" : ""} />
          {refresh.isPending ? "刷新中…" : "立即刷新"}
        </Button>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        {Object.entries(q.data?.current ?? {}).map(([name, value]) => (
          <Card key={name}>
            <CardHeader>
              <CardDescription>{LABELS[name]?.title ?? name}</CardDescription>
              <CardTitle className="text-2xl tabular-nums">{value}</CardTitle>
            </CardHeader>
            <CardContent>
              <code className="text-xs text-[var(--color-muted-foreground)]">
                {LABELS[name]?.usage}
              </code>
            </CardContent>
          </Card>
        ))}
      </div>

      <Card>
        <CardHeader>
          <CardTitle>抓取记录</CardTitle>
          <CardDescription>最近一次抓取的时间与结果</CardDescription>
        </CardHeader>
        <CardContent>
          {records.length === 0 ? (
            <p className="py-6 text-center text-sm text-[var(--color-muted-foreground)]">
              尚无记录（服务启动后会立即抓取一次）
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>目标</TableHead>
                  <TableHead>版本</TableHead>
                  <TableHead>抓取时间</TableHead>
                  <TableHead>结果</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {records.map((r) => (
                  <TableRow key={r.Name}>
                    <TableCell className="font-medium">{LABELS[r.Name]?.title ?? r.Name}</TableCell>
                    <TableCell className="tabular-nums">{r.Value}</TableCell>
                    <TableCell className="text-xs text-[var(--color-muted-foreground)]">
                      {fmtTime(r.FetchedAt)}
                    </TableCell>
                    <TableCell>
                      {r.OK ? (
                        <Badge variant="success">
                          <CircleCheck className="h-3 w-3" /> 成功
                        </Badge>
                      ) : (
                        <Badge variant="destructive" title={r.Error}>
                          <CircleAlert className="h-3 w-3" /> 失败（沿用上一次的值）
                        </Badge>
                      )}
                      {!r.OK && r.Error && (
                        <div className="mt-1 max-w-[420px] truncate text-xs text-[var(--color-muted-foreground)]" title={r.Error}>
                          {r.Error}
                        </div>
                      )}
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
