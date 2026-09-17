# Scheduled tasks and room backups

[简体中文](automation.md)

Select a room in **Scheduled tasks** and add a task. Game commands support existing command templates, including custom templates, and multiline Lua scripts. Commands run in the selected world. Administrator-created schedules support all command risk levels and retain both task results and console records.

In **Backups**, select a room to add independent backup schedules, edit their time and time zone, or pause them. Use five-field Cron (minute, hour, day, month, weekday), or six fields with seconds first. For example, `0 4 * * *` runs daily at 04:00; `0 */6 * * *` runs every six hours. **Tasks and history** opens the execution records.

Scheduled backups include every world in the room. Local rooms use consistent snapshots. Remote or split rooms use hot snapshots when all worlds are running. Partially running rooms briefly stop and resume their previously running worlds; stopped worlds remain stopped. A failed hot backup records an error without automatically switching to a stop-and-backup operation. Creating schedules does not prune old backups; configure cleanup separately.

The management service runs schedules even when the browser is closed. Tasks do not run while that service is stopped.
