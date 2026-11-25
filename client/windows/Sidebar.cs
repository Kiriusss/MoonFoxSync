using System;
using System.Collections.Generic;
using System.Reflection;
using System.Windows;
using System.Windows.Automation;
using System.Windows.Controls;
using System.Windows.Data;
using System.Windows.Markup;
using System.Windows.Media;
using System.Windows.Media.Imaging;
using System.Windows.Shapes;

namespace XinhuSync
{
internal sealed class Sidebar : UserControl
{
    readonly Grid layout;
    readonly Dictionary<string, Button> pages = new Dictionary<string, Button>();
    readonly List<TextBlock> captions = new List<TextBlock>();
    readonly Button toggle;
    bool compact;

    internal event Action<int> WidthChanged;

    internal Sidebar(Action<string> navigate)
    {
        var assembly = Assembly.GetExecutingAssembly();
        using (var source = assembly.GetManifestResourceStream("Sidebar.xaml"))
            layout = (Grid)XamlReader.Load(source);
        Content = layout;
        using (var source = assembly.GetManifestResourceStream("xinhu64.png"))
            ((Image)layout.FindName("Avatar")).Source = BitmapFrame.Create(
                source, BitmapCreateOptions.None, BitmapCacheOption.OnLoad);
        ((TextBlock)layout.FindName("MachineName")).Text = Environment.MachineName;
        var main = (StackPanel)layout.FindName("Pages");
        var utilities = (StackPanel)layout.FindName("Utilities");
        AddPage(main, "overview", "概览", "M3,3 H10 V10 H3 Z M14,3 H21 V10 H14 Z M3,14 H10 V21 H3 Z M14,14 H21 V21 H14 Z", navigate);
        AddPage(main, "projects", "项目", "M2,6 Q2,4 4,4 H9 L12,7 H20 Q22,7 22,9 V18 Q22,20 20,20 H4 Q2,20 2,18 Z", navigate);
        AddPage(main, "libraries", "资料库", "M3,4 H7 V20 H3 Z M10,4 H13 V20 H10 Z M15,6 L18,5 22,19 19,20 Z", navigate);
        AddPage(main, "queue", "同步队列", "M8,5 H21 M8,12 H21 M8,19 H18 M2,5 H3 M2,12 H3 M2,19 H3", navigate);
        AddPage(main, "cache", "下载缓存", "M12,2 V15 M7,10 L12,15 17,10 M3,16 V21 H21 V16", navigate);
        AddPage(utilities, "settings", "设置", "M4,3 V8 M4,13 V21 M2,10 H6 M12,3 V14 M12,19 V21 M10,16 H14 M20,3 V6 M20,11 V21 M18,8 H22", navigate);
        toggle = MakeButton("收起侧栏", "M2,4 H22 V20 H2 Z M9,4 V20 M17,9 L14,12 17,15");
        toggle.Click += delegate { SetCompact(!compact); };
        utilities.Children.Add(toggle);
    }

    Button MakeButton(string title, string path)
    {
        var b = new Button { Style = (System.Windows.Style)layout.FindResource("RailButton"), ToolTip = title };
        AutomationProperties.SetName(b, title);
        var row = new StackPanel { Orientation = Orientation.Horizontal };
        var icon = new Path {
            Data = Geometry.Parse(path), Width = 19, Height = 19, Stretch = Stretch.Uniform,
            StrokeThickness = 1.65, StrokeLineJoin = PenLineJoin.Round,
            StrokeStartLineCap = PenLineCap.Round, StrokeEndLineCap = PenLineCap.Round,
            VerticalAlignment = VerticalAlignment.Center
        };
        icon.SetBinding(Shape.StrokeProperty, new Binding("Foreground") { Source = b });
        var caption = new TextBlock { Text = title, Margin = new Thickness(12, 0, 0, 0),
                                      VerticalAlignment = VerticalAlignment.Center };
        captions.Add(caption);
        row.Children.Add(icon);
        row.Children.Add(caption);
        b.Content = row;
        return b;
    }

    void AddPage(StackPanel host, string key, string title, string icon, Action<string> navigate)
    {
        var button = MakeButton(title, icon);
        button.Click += delegate { navigate(key); };
        pages.Add(key, button);
        host.Children.Add(button);
    }

    void SetCompact(bool value)
    {
        compact = value;
        var visible = compact ? Visibility.Collapsed : Visibility.Visible;
        foreach (var label in captions)
            label.Visibility = visible;
        ((UIElement)layout.FindName("DeviceText")).Visibility = visible;
        ((UIElement)layout.FindName("Brand")).Visibility = visible;
        string action = compact ? "展开侧栏" : "收起侧栏";
        toggle.ToolTip = action;
        AutomationProperties.SetName(toggle, action);
        var changed = WidthChanged;
        if (changed != null)
            changed(compact ? 66 : 192);
    }

    internal string SelectPage(string key)
    {
        foreach (var page in pages)
            page.Value.Tag = page.Key == key ? "selected" : null;
        return key == "overview" ? "同步概览" : AutomationProperties.GetName(pages[key]);
    }
}
}
