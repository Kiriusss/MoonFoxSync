using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class MainWindow
{
    readonly TextBox uploadLimit = new TextBox { Width = 55, AccessibleName = "同时上传文件数" };
    readonly TextBox downloadLimit = new TextBox { Width = 55, AccessibleName = "同时下载文件数" };
    readonly Label transferLimitStatus = Style.Text("", 9, false, Style.Muted);
    Button saveTransferLimits;
    int savedUploadLimit, savedDownloadLimit;

    Control TransferSettingsCard()
    {
        var card = new Surface { Padding = new Padding(16, 10, 16, 10), Margin = new Padding(0, 12, 0, 0) };
        var grid = Column();
        Row(grid, Occupy(Style.Text("项目内并行传输", 12, true, Style.Ink)), 28);
        Row(grid, Occupy(Style.Text("每个项目可同时上传或下载 1 至 50 个文件。新设置从下一次传输开始生效。", 9, false, Style.Muted)), 26);
        var fields = Bar();
        fields.Controls.Add(Hint("同时上传"));
        fields.Controls.Add(uploadLimit);
        fields.Controls.Add(Hint("同时下载"));
        fields.Controls.Add(downloadLimit);
        saveTransferLimits = Action("保存并发数", SaveTransferLimits, true);
        fields.Controls.Add(saveTransferLimits);
        Row(grid, fields, 40);
        Row(grid, Occupy(transferLimitStatus), 23);
        card.Controls.Add(grid);
        uploadLimit.Enabled = downloadLimit.Enabled = saveTransferLimits.Enabled = false;
        uploadLimit.TextChanged += delegate { CheckTransferLimits(); };
        downloadLimit.TextChanged += delegate { CheckTransferLimits(); };
        return card;
    }

    static bool ParseTransferLimit(string value, out int n)
    {
        return Int32.TryParse(value, NumberStyles.None, CultureInfo.InvariantCulture, out n) && n >= 1 && n <= 50;
    }

    void CheckTransferLimits()
    {
        if (savedUploadLimit == 0) return;
        int up, down;
        bool valid = ParseTransferLimit(uploadLimit.Text, out up);
        valid = ParseTransferLimit(downloadLimit.Text, out down) && valid;
        saveTransferLimits.Enabled = valid && (up != savedUploadLimit || down != savedDownloadLimit);
        transferLimitStatus.Text = valid ? "" : "请输入 1 至 50 的整数。";
    }

    void ApplyTransferLimits(Record result)
    {
        int up, down;
        if (!ParseTransferLimit(result.Field("uploads"), out up) || !ParseTransferLimit(result.Field("downloads"), out down))
            throw new InvalidDataException("读取的并发数超出范围。");
        savedUploadLimit = up;
        savedDownloadLimit = down;
        uploadLimit.Text = up.ToString(CultureInfo.InvariantCulture);
        downloadLimit.Text = down.ToString(CultureInfo.InvariantCulture);
        uploadLimit.Enabled = downloadLimit.Enabled = true;
        CheckTransferLimits();
    }

    async Task LoadTransferLimits()
    {
        try { ApplyTransferLimits(await Bridge.Call(null, "settings", "transfers")); }
        catch (Exception ex)
        {
            savedUploadLimit = savedDownloadLimit = 0;
            uploadLimit.Clear();
            downloadLimit.Clear();
            uploadLimit.Enabled = downloadLimit.Enabled = saveTransferLimits.Enabled = false;
            transferLimitStatus.Text = "读取并发数失败：" + ex.Message;
        }
    }

    async Task SaveTransferLimits()
    {
        int up, down;
        if (!ParseTransferLimit(uploadLimit.Text, out up) || !ParseTransferLimit(downloadLimit.Text, out down)) return;
        var args = new List<string> { "settings", "transfers" };
        if (up != savedUploadLimit) args.AddRange(new[] { "--uploads", up.ToString(CultureInfo.InvariantCulture) });
        if (down != savedDownloadLimit) args.AddRange(new[] { "--downloads", down.ToString(CultureInfo.InvariantCulture) });
        ApplyTransferLimits(await Bridge.Call(null, args.ToArray()));
        transferLimitStatus.Text = "已保存";
    }
}
}
