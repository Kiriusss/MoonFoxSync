using System;
using System.Collections;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Text;
using System.Threading.Tasks;
using System.Web.Script.Serialization;

namespace XinhuSync
{
internal sealed class Record : Dictionary<string, object>
{
    public Record(IDictionary<string, object> data) : base(data)
    {
    }
    public string Field(string key)
    {
        object value;
        return TryGetValue(key, out value) && value != null ? Convert.ToString(value) : "";
    }
    public List<Record> Rows(string key = "items")
    {
        object items;
        if (!TryGetValue(key, out items) || !(items is IEnumerable))
            throw new InvalidDataException("客户端返回的清单格式错误");
        return ((IEnumerable)items).Cast<IDictionary<string, object>>().Select(x => new Record(x)).ToList();
    }
}

internal static class Bridge
{
    // CommandLineToArgvW rules, including quotes and trailing backslashes.
    // CreateProcess receives an executable path directly and never opens a shell.
    internal static string Quote(string value)
    {
        var result = new StringBuilder().Append('"');
        int slashes = 0;
        foreach (char c in value)
        {
            if (c == '\\')
            {
                slashes++;
                continue;
            }
            if (c == '"')
                result.Append('\\', 2 * slashes + 1).Append('"');
            else
                result.Append('\\', slashes).Append(c);
            slashes = 0;
        }
        return result.Append('\\', 2 * slashes).Append('"').ToString();
    }

    public static Task<Record> Call(string input, params string[] args)
    {
        return Task.Run(async delegate {
            string exe = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "kdrive.exe");
            if (!File.Exists(exe))
                throw new FileNotFoundException("安装目录缺少 kdrive.exe");
            var utf8 = new UTF8Encoding(false);
            var start = new ProcessStartInfo(exe, String.Join(" ", args.Select(Quote))) {
                UseShellExecute = false,      CreateNoWindow = true,        RedirectStandardOutput = true,
                RedirectStandardError = true, RedirectStandardInput = true, StandardOutputEncoding = utf8,
                StandardErrorEncoding = utf8
            };
            using (var proc = new Process { StartInfo = start })
            {
                proc.Start();
                var stdout = proc.StandardOutput.ReadToEndAsync();
                var stderr = proc.StandardError.ReadToEndAsync();
                if (input != null)
                {
                    byte[] raw = utf8.GetBytes(input + "\n");
                    try
                    {
                        proc.StandardInput.BaseStream.Write(raw, 0, raw.Length);
                    }
                    finally
                    {
                        Array.Clear(raw, 0, raw.Length);
                    }
                }
                proc.StandardInput.Close();
                proc.WaitForExit();
                string output = await stdout.ConfigureAwait(false), error = await stderr.ConfigureAwait(false);
                if (proc.ExitCode != 0)
                    throw new InvalidOperationException(error.Trim());
                var json = new JavaScriptSerializer { MaxJsonLength = Int32.MaxValue };
                return new Record(json.Deserialize<Dictionary<string, object>>(output));
            }
        });
    }
}
}
