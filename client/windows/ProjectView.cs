using System;
using System.Collections.Generic;
using System.Linq;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Controls.Primitives;
using System.Windows.Input;
using System.Windows.Markup;

namespace XinhuSync
{
internal sealed class ProjectView : UserControl
{
    sealed class ProjectRow
    {
        public Record Data { get; private set; }
        public string Name { get { return Data.Field("name"); } }
        public string Id { get { return Data.Field("unitId"); } }
        public bool LocalGit { get { return Data.Field("kind") == "git" && Data.Field("path").Length > 0; } }
        public string Location { get { return Data.Field("path").Length > 0 ? Data.Field("path") : "共享资料库"; } }
        public string Nas { get { return "NAS · " + ProjectDisplay.Nas(Data); } }
        public string Git { get { return Data.Field("kind") == "git" ? "Git · " + ProjectDisplay.Git(Data) : ""; } }
        public string LastActivity { get; private set; }
        public string Details { get; private set; }
        public string AccessibleName { get { return Name + "，" + Nas + (Git == "" ? "" : "，" + Git); } }

        public ProjectRow(Record data, Func<string,string> formatTime)
        {
            Data = data;
            LastActivity = formatTime(data.Field("lastActivity"));
            string checkedAt = formatTime(data.Field("gitCheckedAt"));
            Details = String.Join("  ·  ", new[] { Location, Nas, Git,
                LastActivity == "" ? "" : "最近活动 " + LastActivity,
                checkedAt == "" ? "" : "Git 核对 " + checkedAt, data.Field("gitError") }.Where(v => v != ""));
        }
    }

    readonly Grid root;
    readonly ListBox list;
    readonly TextBox search;
    readonly Button open, more;
    readonly TextBlock warning, detail;
    readonly ContextMenu menu = new ContextMenu();
    readonly List<MenuItem> gitActions = new List<MenuItem>();
    readonly Func<string,string> formatTime;
    List<Record> source;
    List<ProjectRow> rows = new List<ProjectRow>();

    public ProjectView(Action importAction, Action openAction, Func<string,string> formatTime)
    {
        this.formatTime = formatTime;
        using (var stream = typeof(ProjectView).Assembly.GetManifestResourceStream("ProjectView.xaml"))
            root = (Grid)XamlReader.Load(stream);
        Content = root;
        list = (ListBox)root.FindName("List");
        search = (TextBox)root.FindName("Search");
        open = (Button)root.FindName("Open");
        more = (Button)root.FindName("More");
        warning = (TextBlock)root.FindName("Warning");
        detail = (TextBlock)root.FindName("Detail");
        ((Button)root.FindName("Import")).Click += delegate { importAction(); };
        open.Click += delegate { openAction(); };
        more.Click += delegate {
            menu.PlacementTarget = more;
            menu.Placement = PlacementMode.Bottom;
            menu.IsOpen = true;
        };
        list.PreviewMouseRightButtonDown += delegate(object sender, MouseButtonEventArgs e) {
            var row = ContainerAt(e.OriginalSource);
            if (row == null) return;
            row.IsSelected = true;
            row.Focus();
        };
        list.ContextMenu = menu;
        list.ContextMenuOpening += delegate(object sender, ContextMenuEventArgs e) {
            if (Selected == null || (e.CursorLeft >= 0 && ContainerAt(e.OriginalSource) == null))
                e.Handled = true;
            else
            {
                menu.ClearValue(ContextMenu.PlacementTargetProperty);
                menu.ClearValue(ContextMenu.PlacementProperty);
            }
        };
        list.MouseDoubleClick += delegate(object sender, MouseButtonEventArgs e) {
            if (e.ChangedButton == MouseButton.Left && ContainerAt(e.OriginalSource) != null && Selected != null)
            {
                e.Handled = true;
                openAction();
            }
        };
        list.PreviewKeyDown += delegate(object sender, KeyEventArgs e) {
            if (e.Key == Key.Enter && Selected != null)
            {
                e.Handled = true;
                openAction();
            }
        };
        search.TextChanged += delegate { Filter(Selected == null ? null : Selected.Field("unitId")); };
        list.SelectionChanged += delegate { UpdateSelection(); };
        Filter(null);
    }

    ListBoxItem ContainerAt(object origin)
    {
        var node = origin as DependencyObject;
        return node == null ? null : ItemsControl.ContainerFromElement(list, node) as ListBoxItem;
    }

    public void AddAction(string label, Action action, bool gitOnly = false)
    {
        var item = new MenuItem { Header = label };
        item.Click += delegate { if (Selected != null) action(); };
        menu.Items.Add(item);
        if (gitOnly)
        {
            gitActions.Add(item);
            item.IsEnabled = false;
        }
    }

    public void AddSeparator() { menu.Items.Add(new Separator()); }

    public Record Selected
    {
        get
        {
            var row = list.SelectedItem as ProjectRow;
            return row == null ? null : row.Data;
        }
    }

    public void SetProjects(List<Record> projects, string error, string requested)
    {
        warning.Text = error ?? "";
        warning.Visibility = warning.Text.Length > 0 ? Visibility.Visible : Visibility.Collapsed;
        if (requested == null && Object.ReferenceEquals(source, projects)) return;
        string id = requested ?? (Selected == null ? null : Selected.Field("unitId"));
        if (!Object.ReferenceEquals(source, projects))
        {
            source = projects;
            rows = projects.Select(r => new ProjectRow(r, formatTime)).ToList();
        }
        if (requested != null && search.Text.Length > 0)
            search.Clear();
        Filter(id);
    }

    void Filter(string id)
    {
        string query = search.Text.Trim();
        var visible = rows.Where(r => r.Name.IndexOf(query, StringComparison.CurrentCultureIgnoreCase) >= 0).ToList();
        list.ItemsSource = visible;
        var selected = id == null ? null : visible.FirstOrDefault(r => r.Id == id);
        list.SelectedItem = selected;
        if (selected != null) list.ScrollIntoView(selected);
        ((TextBlock)root.FindName("SearchHint")).Visibility = search.Text.Length == 0 ? Visibility.Visible : Visibility.Collapsed;
        ((StackPanel)root.FindName("Empty")).Visibility = visible.Count == 0 ? Visibility.Visible : Visibility.Collapsed;
        ((TextBlock)root.FindName("EmptyTitle")).Text = query == "" ? "还没有项目" : "没有匹配的项目";
        ((TextBlock)root.FindName("EmptyHint")).Text = query == "" ? "配对 NAS 后，导入包含项目的父目录。" : "试试其他项目名称。";
        UpdateSelection();
    }

    void UpdateSelection()
    {
        var row = list.SelectedItem as ProjectRow;
        open.IsEnabled = more.IsEnabled = row != null;
        foreach (var action in gitActions) action.IsEnabled = row != null && row.LocalGit;
        detail.Text = row == null ? String.Format("{0} 个项目", list.Items.Count) : row.Details;
        detail.ToolTip = detail.Text;
    }
}
}
