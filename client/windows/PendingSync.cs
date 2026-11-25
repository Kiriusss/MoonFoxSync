using System;
using System.Collections;
using System.Collections.Generic;
using System.Linq;
using System.Threading.Tasks;

namespace XinhuSync
{
internal sealed partial class WindowsShell
{
    public bool SyncingPending { get; private set; }
    public string SyncResult { get; private set; }
    public string SyncErrorDetails { get; private set; }
    public bool CanSyncPending { get { return !SyncingPending && Queued > 0; } }
    public string SyncButtonText { get { return SyncingPending ? "同步中…" : "立即同步"; } }

    public async Task SyncPending()
    {
        if (quitting || !CanSyncPending)
            return;
        SyncingPending = true;
        SyncResult = "正在上传待传快照…";
        SyncErrorDetails = "";
        Notify();
        try
        {
            var args = new List<string> { "sync-pending", "--now" };
            policy.AppendHomeRoute(args);
            var result = await Bridge.Call(null, args.ToArray());
            var counts = new List<string>();
            foreach (var field in new[] {
                new { Key = "uploaded", Label = "已上传" }, new { Key = "active", Label = "其他任务正在传输" },
                new { Key = "skipped", Label = "已跳过" }, new { Key = "failed", Label = "失败" }
            })
            {
                int count = Convert.ToInt32(result[field.Key]);
                if (count > 0)
                    counts.Add(field.Label + " " + count + " 项");
            }
            SyncResult = Convert.ToInt32(result["total"]) == 0 ? "队列中没有待上传的快照" : String.Join(" · ", counts);
            var errors = result["errors"] as IEnumerable;
            if (errors != null)
                SyncErrorDetails = String.Join(Environment.NewLine, errors.Cast<string>());
        }
        catch (Exception ex)
        {
            SyncResult = "同步失败";
            SyncErrorDetails = ex.Message;
        }
        // Read after any earlier count request so its pre-upload state cannot win.
        if (queueRead != null)
            await queueRead;
        queueRead = ReadQueue();
        await queueRead;
        SyncingPending = false;
        Notify();
        RequestProjects();
    }
}
}
