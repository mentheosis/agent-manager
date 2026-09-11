import { createActivityRow } from "../am-controller-activity.js";
import { queueFilters } from "./view-config.js";
export function appendQueueLogs(output, logs, title, filters) {
  for (const log of logs) {
    const task = log.task_id;
    const row = createActivityRow(
      {
        event_type: log.event_type,
        actor: log.actor || "Controller",
        target: task ? `Task ${task}` : null,
        ts: log.created_at,
        text:
          log.summary +
          (log.data ? "\n\n" + JSON.stringify(log.data, null, 2) : ""),
      },
      queueFilters,
      filters,
    );
    output.append(row);
  }
}
