using System;
using System.Drawing;
using System.Globalization;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class MainWindow
{
    readonly ComboBox codexPolicy = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Width = 180 };
    readonly ComboBox codexScope = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Width = 320 };
    string fixedCodexScope = "";
    readonly NumericUpDown codexInterval =
        new NumericUpDown { Minimum = 1, Maximum = 1440, Value = 30, Width = 90 };
    readonly Label codexState = Style.Text("正在读取配置…", 9, true, Style.Muted),
                   codexRoot = Style.Text("", 9, false, Style.Muted),
                   codexLast = Style.Text("", 9, false, Style.Muted),
                   codexError = Style.Text("", 9, false, Style.Plum);
    readonly string[] policyCodes = { "home", "network", "manual" };
    Button codexSave, codexStop, codexNow;
    Control CodexCard()
    {
        var card = new Surface { Padding = new Padding(16, 10, 16, 10), Margin = new Padding(0, 12, 0, 0) };
        var inner = Column();
        Row(inner, Occupy(Style.Text("Codex 对话历史", 12, true, Style.Ink)), 30);
        Row(inner, Occupy(codexState), 24);
        Row(inner, Occupy(codexRoot), 24);
        var target = Bar();
        target.Controls.Add(Hint("备份资料库"));
        target.Controls.Add(codexScope);
        Row(inner, target, 36);
        Row(inner,
            Occupy(Style.Text("将保存对话及工具输出，可由目标资料库的授权设备读取。", 9, false, Style.Muted)),
            24);
        Row(inner,
            Occupy(Style.Text("自动备份跟随全局自动检查开关；暂停时仍可立即备份。", 9, false, Style.Muted)),
            24);
        var options = Bar();
        codexPolicy.Items.AddRange(
            new object[] { "仅在家庭网络自动备份", "跟随当前同步网络策略", "仅手动备份" });
        codexPolicy.SelectedIndex = 1;
        options.Controls.Add(Hint("备份策略"));
        options.Controls.Add(codexPolicy);
        options.Controls.Add(Hint("检查间隔（分钟）"));
        options.Controls.Add(codexInterval);
        Row(inner, options, 38);
        var actions = Bar();
        codexSave = Action("启用备份", async delegate {
            if (codexPolicy.SelectedIndex < 0)
                return;
            var choice = codexScope.SelectedItem as Choice;
            string scope = String.IsNullOrEmpty(fixedCodexScope)
                               ? (choice == null ? "" : choice.Row.Field("unitId"))
                               : fixedCodexScope;
            if (String.IsNullOrEmpty(scope))
                throw new InvalidOperationException("请选择备份资料库。");
            await Bridge.Call(null, "preset", "codex", "enable", "--policy",
                              policyCodes[codexPolicy.SelectedIndex], "--interval",
                              codexInterval.Value.ToString(CultureInfo.InvariantCulture), "--scope", scope);
            await LoadCodexSettings();
            status.Text = "Codex 备份配置已保存";
        }, true);
        codexNow = Action("立即备份", async delegate {
            Exception failure = null;
            try
            {
                await Bridge.Call(null, "preset", "codex", "run");
            }
            catch (Exception ex)
            {
                failure = ex;
            }
            await LoadCodexSettings();
            if (failure != null)
                throw new InvalidOperationException(failure.Message, failure);
            status.Text = "Codex 备份检查完成";
        });
        codexStop = Action("停止自动备份", async delegate {
            await Bridge.Call(null, "preset", "codex", "disable");
            await LoadCodexSettings();
            status.Text = "已停止 Codex 自动备份";
        });
        foreach (var b in new[] { codexSave, codexNow, codexStop })
        {
            b.Enabled = false;
            actions.Controls.Add(b);
        }
        Row(inner, actions, 40);
        Row(inner, Occupy(codexLast), 24);
        Row(inner, Occupy(codexError), 32);
        codexScope.SelectedIndexChanged += delegate
        {
            codexSave.Enabled = codexScope.SelectedItem is Choice;
        };
        card.Controls.Add(inner);
        return card;
    }
    async Task LoadCodexSettings()
    {
        Record value;
        try
        {
            value = await Bridge.Call(null, "preset", "codex", "status");
        }
        catch (Exception ex)
        {
            codexState.Text = "暂时无法读取备份配置";
            codexError.Text = ex.Message;
            codexSave.Enabled = codexNow.Enabled = codexStop.Enabled = false;
            return;
        }
        bool enabled = String.Equals(value.Field("enabled"), "true", StringComparison.OrdinalIgnoreCase);
        int i = Array.IndexOf(policyCodes, value.Field("policy"));
        codexPolicy.SelectedIndex = i >= 0 ? i : 1;
        int minutes;
        if (!Int32.TryParse(value.Field("intervalMinutes"), out minutes) || minutes <= 0)
            minutes = 30;
        codexInterval.Value = Math.Min(1440, minutes);
        codexState.Text = enabled ? "已启用" : "未启用";
        codexRoot.Text =
            String.IsNullOrWhiteSpace(value.Field("root")) ? "" : "来源目录：" + value.Field("root");
        codexLast.Text = String.IsNullOrEmpty(value.Field("lastBackup"))
                             ? "尚无已完成的备份"
                             : "最近备份：" + LocalTime(value.Field("lastBackup"));
        codexError.Text = value.Field("lastError");
        codexSave.Text = enabled ? "保存备份配置" : "启用备份";
        fixedCodexScope = value.Field("scopeId");
        codexScope.Items.Clear();
        codexScope.Enabled = String.IsNullOrEmpty(fixedCodexScope);
        codexNow.Enabled = !String.IsNullOrEmpty(value.Field("unitId"));
        codexStop.Enabled = enabled;
        if (!String.IsNullOrEmpty(fixedCodexScope))
        {
            var found = libraries.Find(r => r.Field("unitId") == fixedCodexScope);
            var saved = found ?? new Record(new System.Collections.Generic.Dictionary<string, object> {
                            { "unitId", fixedCodexScope }
                        });
            codexScope.Items.Add(
                new Choice(saved, (found == null ? "" : found.Field("name") + " · ") + fixedCodexScope));
            codexScope.SelectedIndex = 0;
            codexSave.Enabled = true;
        }
        else
        {
            codexScope.Items.Add("请选择用于备份的资料库");
            try
            {
                foreach (var unit in (await Bridge.Call(null, "units")).Rows())
                {
                    if (unit.Field("kind") == "managed" && unit.Field("role") == "sync" &&
                        String.IsNullOrEmpty(unit.Field("scopeId")))
                        codexScope.Items.Add(
                            new Choice(unit, unit.Field("name") + " · " + unit.Field("unitId")));
                }
            }
            catch (Exception ex)
            {
                codexError.Text = ex.Message;
            }
            codexScope.SelectedIndex = codexScope.Items.Count == 2 ? 1 : 0;
            codexSave.Enabled = codexScope.SelectedItem is Choice;
        }
    }
}
}
