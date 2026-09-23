import { Badge } from "@/components/ui/badge";
import type { SystemOneTestQuestion } from "@/lib/api";
import { t } from "@/lib/i18n";

export function SystemOneTestQuestions({ questions }: { questions: SystemOneTestQuestion[] | undefined }) {
  if (!questions?.length) return null;
  return (
    <div className="grid gap-2 sm:grid-cols-3" aria-live="polite">
      {questions.map((question) => (
        <div key={question.type} className="flex flex-col gap-1 rounded-md bg-muted/40 p-2 text-xs">
          <div className="flex flex-wrap items-center gap-2">
            <span>{question.type === "choice" ? "Choice" : question.type === "noul" ? "Noul" : "Score"}</span>
            <Badge variant="outline" className={question.ok ? "text-signal-ok" : "text-signal-alert"}>
              {question.ok ? t("通过") : t("失败")}
            </Badge>
          </div>
          {question.choice !== undefined ? <span>{t("选择：{choice}", { choice: question.choice === "billing" ? t("账单和退款") : t("技术故障") })}</span> : null}
          {question.noul !== undefined ? <span>{t("为真概率：{value}%", { value: (question.noul * 100).toFixed(1) })}</span> : null}
          {question.score !== undefined ? <span>{t("评分：{value} / 2", { value: question.score.toFixed(3) })}</span> : null}
          {question.message ? <span className="text-destructive">{question.message}</span> : null}
        </div>
      ))}
    </div>
  );
}
