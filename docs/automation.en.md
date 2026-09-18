# Scheduled tasks and room backups

[简体中文](automation.md)

Select a room in **Scheduled tasks** and add a task. Game commands support existing command templates, including custom templates, and multiline Lua scripts. Commands run in the selected world. Administrator-created schedules support all command risk levels and retain both task results and console records.

Enable mod and game server updates separately in **Room settings → Automatic maintenance**. Both are opt-in. New schedules check every 15 minutes; adjust the interval or use a custom Cron expression for game updates. The mod policy is shared with **Mod management → Update policy** and defaults to a 60-second empty-room grace period.

When an update is available, the system actively checks every affected room. Online players or unavailable presence information defer maintenance. Rooms sharing an installation are checked together; game installations for split rooms are updated together to keep Master and Caves on compatible builds. Mods download while empty and are rechecked after the grace period before restarting. Game updates create protection backups first, then update and verify startup. Only previously running worlds resume; no update means no restart. An interrupted game release blocks further automatic updates for those installations until you recover it in **Game server management**, preserving the original recovery records.

In **Backups**, select a room to add independent backup schedules, edit their time and time zone, or pause them. Use five-field Cron (minute, hour, day, month, weekday), or six fields with seconds first. For example, `0 4 * * *` runs daily at 04:00; `0 */6 * * *` runs every six hours. **Tasks and history** opens the execution records.

Scheduled backups include every world in the room. Local, remote, and split rooms all use hot snapshots when all worlds are running. Partially running rooms briefly stop and resume their previously running worlds; stopped worlds remain stopped. A failed hot backup records an error without automatically switching to a stop-and-backup operation. Schedules can retain the newest 1–100 automatic snapshots; new schedules default to 7. Leave retention blank to disable cleanup. Cleanup runs only after a successful backup and preserves manual, protection, and recovery-referenced backups. Retention covers snapshots from all schedules in the room, so use the same count across schedules. Legacy ZIP snapshots and new backup sets are counted separately. Existing schedules without retention keep their previous behavior.

The management service runs schedules even when the browser is closed. Tasks do not run while that service is stopped. Cancellation or timeout stops cleanup before the next backup. A deletion already in progress finishes its commit; deleted backups are not automatically restored.

System Settings links to per-room schedules instead of applying a global backup switch. Previously enabled interval policies keep running and can be viewed and paused in the room backup schedule panel.

New backup sets support download and deletion. Downloads are portable room ZIPs containing shared files such as `cluster.ini` and every world directory; they can be uploaded through Save Imports. Shared configuration comes from Master, so check networking for the destination machines after importing a split room. Deletion requires the exact backup name and is blocked for backups still needed by restore, mod publication, or game update operations.

Manual backups in the UI use the consistency coordinator. Existing ZIP backups remain available to view, download, restore, and delete. The legacy ZIP creation endpoint requires a stopped room.

Upload staging uses the corresponding feature's data directory; reserve disk space for the upload and processing. HTTP upload reads allow up to two hours. Configure any reverse proxy with matching size and time limits. Temporary upload and export files left after a crash are reclaimed at startup or on the next operation of the same type. Files used by active uploads or downloads are protected.
