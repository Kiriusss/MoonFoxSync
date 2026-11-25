using System;
using System.Collections.Generic;
using System.Linq;
using System.Windows;
using System.Windows.Automation;
using System.Windows.Controls;
using System.Windows.Markup;
using System.Windows.Media;
using System.Windows.Shapes;

namespace XinhuSync
{
internal sealed class Dashboard : UserControl
{
    readonly FrameworkElement view;
    readonly TextBlock connection, endpoint, libraryCount, queueCount, syncMode, emptyTitle, emptyMessage, warning;
    readonly FrameworkElement emptyPanel, leds;
    readonly Button serverAction, libraries, queue, settings, import;
    readonly Ellipse dot;
    readonly ItemsControl recentItems;
    static readonly Brush OnlineLight = new SolidColorBrush(Color.FromRgb(135, 222, 194)),
                          OfflineLight = new SolidColorBrush(Color.FromRgb(242, 181, 191));
    bool paired, online, projectsPaired;
    List<Record> renderedProjects;
    string renderedWarning;

    public Dashboard(Action<string> navigate, Action<string> selectProject,
                     Action pair, Action reconnect, Action importProjects)
    {
        using (var stream = typeof(Dashboard).Assembly.GetManifestResourceStream("Dashboard.xaml"))
            view = (FrameworkElement)XamlReader.Load(stream);
        Content = view;
        connection = Part<TextBlock>("ConnectionLabel");
        endpoint = Part<TextBlock>("Endpoint");
        libraryCount = Part<TextBlock>("LibraryCount");
        queueCount = Part<TextBlock>("QueueCount");
        syncMode = Part<TextBlock>("SyncMode");
        dot = Part<Ellipse>("ConnectionDot");
        leds = Part<FrameworkElement>("Leds");
        leds.DataContext = OfflineLight;
        emptyTitle = Part<TextBlock>("EmptyTitle");
        emptyMessage = Part<TextBlock>("EmptyMessage");
        warning = Part<TextBlock>("ProjectsWarning");
        emptyPanel = Part<FrameworkElement>("EmptyPanel");
        recentItems = Part<ItemsControl>("RecentItems");
        libraries = Part<Button>("Libraries");
        queue = Part<Button>("Queue");
        settings = Part<Button>("Settings");
        import = Part<Button>("Import");
        serverAction = Part<Button>("ServerAction");

        libraries.Click += delegate { navigate("libraries"); };
        queue.Click += delegate { navigate("queue"); };
        settings.Click += delegate { navigate("settings"); };
        Part<Button>("AllProjects").Click += delegate { navigate("projects"); };
        import.Click += delegate { importProjects(); };
        serverAction.Click += delegate {
            if (!paired) pair();
            else if (online) navigate("libraries");
            else reconnect();
        };
        recentItems.AddHandler(Button.ClickEvent, new RoutedEventHandler(delegate(object sender, RoutedEventArgs e) {
            var button = e.Source as Button;
            if (button != null && button.Tag is string)
                selectProject((string)button.Tag);
        }));
    }

    T Part<T>(string name) where T : FrameworkElement
    {
        return (T)view.FindName(name);
    }

    public void SetStatus(string status, string url, bool isPaired, bool reachable,
                          string libraryTotal, string pendingTotal, string automatic)
    {
        paired = isPaired;
        online = paired && reachable;
        connection.Text = status;
        endpoint.Text = String.IsNullOrEmpty(url) ? "使用 NAS 签发的配对码连接" : url;
        endpoint.ToolTip = endpoint.Text;
        dot.Fill = online ? OnlineLight : OfflineLight;
        leds.DataContext = dot.Fill;
        serverAction.Content = paired ? (online ? "打开资料库" : "重新连接") : "配对 NAS";
        libraryCount.Text = libraryTotal;
        queueCount.Text = pendingTotal;
        syncMode.Text = automatic;
        AutomationProperties.SetName(libraries, "资料库 " + libraryTotal);
        AutomationProperties.SetName(queue, "待上传快照 " + pendingTotal);
        AutomationProperties.SetName(settings, "自动同步 " + automatic);
    }

    public void SetProjects(List<Record> projects, string projectWarning, bool isPaired)
    {
        if (ReferenceEquals(projects, renderedProjects) && projectWarning == renderedWarning && projectsPaired == isPaired)
            return;
        if (!ReferenceEquals(projects, renderedProjects))
            recentItems.ItemsSource = projects.Take(5).Select(p => new {
                Unit = p.Field("unitId"), Name = p.Field("name"),
                Nas = "NAS · " + ProjectDisplay.Nas(p), Git = "Git · " + ProjectDisplay.Git(p),
                Description = "打开项目 " + p.Field("name")
            }).ToList();
        renderedProjects = projects;
        renderedWarning = projectWarning;
        projectsPaired = isPaired;
        bool empty = projects.Count == 0;
        bool error = !String.IsNullOrEmpty(projectWarning);
        // ProjectError is null until the first local project-status request finishes.
        bool loading = empty && projectWarning == null;
        recentItems.Visibility = empty ? Visibility.Collapsed : Visibility.Visible;
        emptyPanel.Visibility = empty ? Visibility.Visible : Visibility.Collapsed;
        emptyTitle.Text = error ? "无法完整读取项目状态" : loading ? "正在读取项目状态" : "还没有项目";
        emptyMessage.Text = error ? projectWarning : loading ? "" : "导入一个项目父目录，或在资料库中绑定现有项目。";
        emptyMessage.Visibility = loading ? Visibility.Collapsed : Visibility.Visible;
        import.Visibility = empty && !error && !loading && isPaired ? Visibility.Visible : Visibility.Collapsed;
        warning.Text = projectWarning;
        warning.Visibility = !empty && error ? Visibility.Visible : Visibility.Collapsed;
    }
}
}
