import { createActivityRow } from "../am-controller-activity.js";
import { queueFilters } from "./view-config.js";
export function appendQueueLogs(output, logs, title, filters) {
  for (const log of logs) {
    const explanation = log.event_type === "task_status" ? log.data : log.data?.handoff;
    const readable = explanation ? [explanation.headline,
      ...[["completed","Completed"],["remaining","Remaining"],["reason","Why"],["next_action","Next action"],["actor","Who acts"],["retry_after","Retry after"],["readiness","Readiness"]]
        .filter(([key]) => explanation[key]).map(([key,label]) => `${label}: ${explanation[key]}`),
      `Source: ${explanation.source || log.actor} · ${log.created_at}`].join("\n\n") : null;
    const task = log.task_id;
    const row = createActivityRow(
      {
        event_type: log.event_type,
        actor: log.actor || "Controller",
        target: task ? `Task ${task}` : null,
        ts: log.created_at,
        text:
          readable || log.summary +
          (log.data ? "\n\n" + JSON.stringify(log.data, null, 2) : ""),
      },
      queueFilters,
      filters,
    );
    output.append(row);
  }
}
