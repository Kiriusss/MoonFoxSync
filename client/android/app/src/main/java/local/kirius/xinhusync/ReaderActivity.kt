package local.kirius.xinhusync

import android.app.Activity
import android.app.AlertDialog
import android.content.ClipData
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.pdf.PdfRenderer
import android.os.Bundle
import android.os.ParcelFileDescriptor
import android.text.InputType
import android.view.Gravity
import android.view.View
import android.webkit.MimeTypeMap
import android.widget.*
import androidx.core.content.FileProvider
import java.io.File
import java.io.IOException
import java.nio.charset.StandardCharsets
import java.util.ArrayDeque
import java.util.UUID
import java.util.concurrent.Executors
import java.util.concurrent.Future
import kotlin.math.max
import org.json.JSONObject

internal sealed class Screen(val title: String) {
    class Units : Screen("资料库")

    class History(val unit: String, title: String) : Screen(title)

    class Entries(val folder: Folder, title: String) : Screen(title)
}

class ReaderActivity : Activity() {
    private lateinit var keys: Credentials
    private lateinit var nas: Nas
    private lateinit var top: LinearLayout
    private lateinit var body: LinearLayout
    private lateinit var status: TextView
    private val worker = Executors.newSingleThreadExecutor()
    private var running: Future<*>? = null
    private var generation = 0
    private var screen: Screen? = null
    private val trail = ArrayDeque<Screen>()
    private var items = mutableListOf<JSONObject>()
    private var cursor: String? = null
    private var pem = ""
    private var certButton: Button? = null
    private var opened: Member? = null
    private val ink = Color.rgb(38, 42, 55)
    private val blue = Color.rgb(41, 76, 117)

    override fun onCreate(saved: Bundle?) {
        super.onCreate(saved)
        keys = Credentials(this)
        nas = Nas(keys)
        pem = keys.pem
        window.statusBarColor = blue
        top =
            LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                setBackgroundColor(Color.rgb(249, 249, 252))
            }
        if (android.os.Build.VERSION.SDK_INT >= 35) {
            top.setOnApplyWindowInsetsListener { view, insets ->
                val bars =
                    insets.getInsets(
                        android.view.WindowInsets.Type.systemBars() or
                            android.view.WindowInsets.Type.ime()
                    )
                view.setPadding(bars.left, bars.top, bars.right, bars.bottom)
                insets
            }
        }
        setContentView(top)
        if (keys.active) navigate(Screen.Units(), false) else pairing()
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()

    private fun text(s: String, size: Int = 15) =
        TextView(this).apply {
            text = s
            textSize = size.toFloat()
            setTextColor(ink)
            setPadding(dp(12), dp(7), dp(12), dp(7))
        }

    private fun button(parent: LinearLayout, s: String, click: () -> Unit): Button {
        val b =
            Button(this).apply {
                text = s
                isAllCaps = false
                setOnClickListener { click() }
            }
        parent.addView(b)
        return b
    }

    private fun frame(title: String, back: Boolean, scroll: Boolean = true) {
        top.removeAllViews()
        certButton = null
        val bar =
            LinearLayout(this).apply {
                gravity = Gravity.CENTER_VERTICAL
                setPadding(dp(12), dp(12), dp(12), dp(8))
            }
        top.addView(bar)
        if (back) button(bar, "返回") { onBackPressed() }
        else
            bar.addView(
                ImageView(this).apply { setImageResource(R.drawable.xinhu) },
                dp(48),
                dp(48),
            )
        bar.addView(
            text(title, 21).apply {
                typeface = Typeface.DEFAULT_BOLD
                maxLines = 2
            },
            LinearLayout.LayoutParams(0, -2, 1f),
        )
        if (keys.active) button(bar, "设备") { settings() }
        status = text("", 13)
        top.addView(status)
        body =
            LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                setPadding(dp(10), 0, dp(10), dp(12))
            }
        if (scroll) {
            val v = ScrollView(this)
            v.addView(body)
            top.addView(v, LinearLayout.LayoutParams(-1, 0, 1f))
        } else top.addView(body, LinearLayout.LayoutParams(-1, 0, 1f))
    }

    private fun cancel() {
        generation++
        running?.cancel(true)
        running = null
        nas.cancel()
    }

    private fun <T> task(label: String, work: () -> T, done: (T) -> Unit) {
        cancel()
        val current = generation
        status.text = label
        running =
            worker.submit {
                try {
                    val result = work()
                    runOnUiThread {
                        if (current == generation && !isDestroyed) {
                            running = null
                            try {
                                done(result)
                            } catch (e: Exception) {
                                status.text = e.localizedMessage
                            }
                        }
                    }
                } catch (e: Exception) {
                    runOnUiThread {
                        if (current == generation && !isDestroyed) {
                            running = null
                            status.text = e.localizedMessage ?: e.javaClass.simpleName
                            if (e is HttpFailure && e.status == 401 && keys.active)
                                forget("设备凭据已失效，请重新配对。")
                        }
                    }
                }
            }
    }

    private fun pairing() {
        opened = null
        screen = null
        frame(getString(R.string.app_name), false)
        status.text = "连接 NAS · 只读设备配对"
        val address =
            EditText(this).apply {
                hint = "https://NAS地址:端口"
                isSingleLine = true
                setText(keys.address)
                inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
            }
        val code =
            EditText(this).apply {
                hint = "一次性只读配对码"
                isSingleLine = true
                inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
                isSaveEnabled = false
            }
        body.addView(address)
        body.addView(code)
        certButton =
            button(body, if (pem.isEmpty()) "选择 NAS CA 证书（可选）" else "更换已选择的 CA 证书") {
                startActivityForResult(
                    Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = "*/*"
                    },
                    11,
                )
            }
        button(body, "配对并打开资料库") {
            val url = address.text.toString().trim()
            val secret = code.text.toString().trim()
            if (secret.isEmpty()) {
                status.text = "请输入配对码"
                return@button
            }
            val selected = pem
            task("正在验证 NAS…", { nas.pair(url, selected, secret, getString(R.string.app_name) + " Android") }) {
                code.text.clear()
                navigate(Screen.Units(), false)
            }
        }
    }

    @Deprecated("Document picker result")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == 11 && resultCode == RESULT_OK) {
            val uri = data?.data ?: return
            task(
                "读取证书…",
                { contentResolver.openInputStream(uri)!!.use { String(bounded(it, 131072)) } },
            ) { selected ->
                require(selected.contains("-----BEGIN CERTIFICATE-----")) { "请选择 PEM 格式的 CA 证书" }
                pem = selected
                certButton?.text = "已选择 CA 证书 · 点击更换"
                status.text = "请核对服务地址并输入配对码"
            }
        }
    }

    private fun forget(reason: String) {
        cancel()
        keys.clear()
        cacheDir.resolve("viewer").deleteRecursively()
        pem = ""
        trail.clear()
        pairing()
        status.text = reason
    }

    private fun settings() {
        AlertDialog.Builder(this)
            .setTitle("此设备")
            .setMessage("${keys.address}\n设备 ID  ${keys.id}")
            .setPositiveButton("关闭", null)
            .setNeutralButton("清理缓存") { _, _ ->
                cancel()
                cacheDir.resolve("viewer").deleteRecursively()
                status.text = "手机预览缓存已清理"
            }
            .setNegativeButton("解除配对") { _, _ ->
                AlertDialog.Builder(this)
                    .setMessage("移除本机设备凭据和缓存？")
                    .setPositiveButton("移除") { _, _ -> forget("已移除本机配对") }
                    .setNegativeButton("取消", null)
                    .show()
            }
            .show()
    }

    private fun navigate(next: Screen, push: Boolean = true) {
        if (push) screen?.let { trail.addLast(it) }
        cancel()
        opened = null
        screen = next
        items = mutableListOf()
        cursor = null
        renderList(next)
        load(next, null)
    }

    private fun load(s: Screen, next: String?) {
        task(
            "加载目录…",
            {
                when (s) {
                    is Screen.Units -> nas.units(next)
                    is Screen.History -> nas.history(s.unit, next)
                    is Screen.Entries -> nas.list(s.folder, next)
                }
            },
        ) { page ->
            val a = page.getJSONArray("items")
            for (i in 0 until a.length()) items.add(a.getJSONObject(i))
            cursor = if (page.isNull("nextCursor")) null else page.getString("nextCursor")
            renderList(s)
        }
    }

    private fun renderList(s: Screen) {
        frame(s.title, !trail.isEmpty(), false)
        status.text =
            when (s) {
                is Screen.Units -> getString(R.string.reader_libraries, getString(R.string.app_name))
                is Screen.History -> "选择要查看的快照"
                is Screen.Entries -> s.folder.path.ifEmpty { "根目录" }
            }
        val labels =
            items.map { j ->
                when (s) {
                    is Screen.Units -> j.getString("name")
                    is Screen.History ->
                        listOf(j.optString("label").ifEmpty { "自动快照" }, j.optString("committedAt"))
                            .joinToString("   ")
                    is Screen.Entries ->
                        (if (j.optString("kind") == "directory") "▸  " else "") +
                            j.getString("path").substringAfterLast('/')
                }
            }
        val list = ListView(this)
        list.adapter = ArrayAdapter(this, android.R.layout.simple_list_item_1, labels)
        list.setOnItemClickListener { _, _, i, _ -> if (i < items.size) select(s, items[i]) }
        body.addView(list, LinearLayout.LayoutParams(-1, 0, 1f))
        cursor?.let { c -> button(body, "加载下一页") { load(s, c) } }
        button(body, "刷新") { navigate(s, false) }
    }

    private fun select(s: Screen, j: JSONObject) {
        when (s) {
            is Screen.Units ->
                if (j.getString("kind") == "external")
                    navigate(
                        Screen.Entries(Folder(j.getString("unitId"), null), j.getString("name"))
                    )
                else navigate(Screen.History(j.getString("unitId"), j.getString("name")))
            is Screen.History ->
                navigate(
                    Screen.Entries(
                        Folder(s.unit, j.getString("snapshotId")),
                        j.optString("label").ifBlank { "项目快照" },
                    )
                )
            is Screen.Entries ->
                when (j.getString("kind")) {
                    "directory" -> {
                        val p = j.getString("path")
                        navigate(Screen.Entries(s.folder.copy(path = p), p.substringAfterLast('/')))
                    }
                    "file" -> {
                        val m =
                            Member(
                                s.folder,
                                j.getString("path"),
                                j.getLong("size"),
                                if (j.isNull("digest")) null else j.getString("digest"),
                            )
                        file(m)
                    }
                    else -> status.text = "此项是链接或挂载项，请打开对应资料库"
                }
        }
    }

    private fun file(m: Member) {
        cancel()
        opened = m
        frame(m.path.substringAfterLast('/'), true)
        status.text = "${m.size} 字节 · ${m.folder.snap?:"当前 NAS 文件"}"
        val ext = m.path.substringAfterLast('.', "").lowercase()
        if (
            ext in
                setOf(
                    "txt",
                    "md",
                    "json",
                    "go",
                    "rs",
                    "js",
                    "ts",
                    "kt",
                    "c",
                    "cpp",
                    "h",
                    "py",
                    "log",
                    "xml",
                    "yaml",
                    "yml",
                    "csv",
                    "tex",
                    "ini",
                    "toml",
                    "sh",
                    "swift",
                    "cs",
                )
        ) {
            val out = java.io.ByteArrayOutputStream()
            var version = m.hash
            val view =
                text("", 14).apply {
                    typeface = Typeface.MONOSPACE
                    setTextIsSelectable(true)
                }
            var more: Button? = null
            fun read() {
                task("按需读取文本…", { nas.preview(m.copy(hash = version), out.size().toLong()) }) {
                    (segment, tag) ->
                    out.write(segment)
                    version = tag
                    val raw = out.toByteArray()
                    val decoder =
                        StandardCharsets.UTF_8.newDecoder()
                            .onMalformedInput(java.nio.charset.CodingErrorAction.REPLACE)
                    val chars = java.nio.CharBuffer.allocate(raw.size)
                    decoder.decode(
                        java.nio.ByteBuffer.wrap(raw),
                        chars,
                        raw.size.toLong() == m.size,
                    )
                    chars.flip()
                    view.text = chars.toString()
                    status.text = "已读 ${raw.size} / ${m.size} 字节"
                    more?.visibility =
                        if (raw.size.toLong() < m.size && raw.size < (1 shl 20)) View.VISIBLE
                        else View.GONE
                }
            }
            more = button(body, "继续读取") { read() }
            button(body, "下载完整文件并打开") { confirmDownload(m.copy(hash = version), ext) }
            body.addView(view)
            read()
        } else button(body, "下载并打开") { confirmDownload(m, ext) }
    }

    private fun confirmDownload(m: Member, ext: String) {
        if (m.size > 10L * 1024 * 1024)
            AlertDialog.Builder(this)
                .setMessage(
                    "本次将下载 ${android.text.format.Formatter.formatFileSize(this,m.size)}，继续打开？"
                )
                .setNegativeButton("取消", null)
                .setPositiveButton("下载") { _, _ -> download(m, ext) }
                .show()
        else download(m, ext)
    }

    private fun download(m: Member, ext: String) {
        val viewerDir = cacheDir.resolve("viewer").resolve(UUID.randomUUID().toString())
        if (!viewerDir.mkdirs()) {
            status.text = "无法创建预览缓存"
            return
        }
        val name =
            m.path
                .substringAfterLast('/')
                .map { ch -> if (ch == '\\' || ch.isISOControl()) '_' else ch }
                .joinToString("")
        val dest =
            viewerDir.resolve(if (name.isBlank() || name == "." || name == "..") "file" else name)
        task(
            "正在下载并校验…",
            {
                nas.download(m, dest)
                dest
            },
        ) { f ->
            status.text = "已校验 ${m.size} 字节"
            when (ext) {
                "pdf" -> pdfPage(f, 0)
                "jpeg",
                "jpg",
                "png",
                "webp",
                "bmp" -> {
                    val opts = BitmapFactory.Options().apply { inJustDecodeBounds = true }
                    BitmapFactory.decodeFile(f.path, opts)
                    opts.inJustDecodeBounds = false
                    opts.inSampleSize = 1
                    while (max(opts.outWidth, opts.outHeight) / opts.inSampleSize > 2048) opts
                        .inSampleSize *= 2
                    val bmp = BitmapFactory.decodeFile(f.path, opts) ?: throw IOException("图像无法打开")
                    body.removeAllViews()
                    body.addView(
                        ImageView(this).apply {
                            adjustViewBounds = true
                            setImageBitmap(bmp)
                        }
                    )
                    button(body, "用其他应用打开") { external(f) }
                }
                else -> external(f)
            }
        }
        button(body, "取消下载") {
            cancel()
            file(m)
            status.text = "已取消下载"
        }
    }

    private fun pdfPage(file: File, index: Int) {
        task(
            "读取 PDF…",
            {
                val renderer =
                    PdfRenderer(
                        ParcelFileDescriptor.open(file, ParcelFileDescriptor.MODE_READ_ONLY)
                    )
                renderer.use { r ->
                    r.openPage(index).use { page ->
                        val scale = 1600.0 / max(page.width, page.height)
                        val bitmap =
                            Bitmap.createBitmap(
                                max(1, (page.width * scale).toInt()),
                                max(1, (page.height * scale).toInt()),
                                Bitmap.Config.ARGB_8888,
                            )
                        bitmap.eraseColor(Color.WHITE)
                        page.render(bitmap, null, null, PdfRenderer.Page.RENDER_MODE_FOR_DISPLAY)
                        bitmap to r.pageCount
                    }
                }
            },
        ) { (bitmap, count) ->
            body.removeAllViews()
            status.text = "PDF · 第 ${index+1} / $count 页"
            body.addView(
                ImageView(this).apply {
                    adjustViewBounds = true
                    setImageBitmap(bitmap)
                }
            )
            if (index > 0) button(body, "上一页") { pdfPage(file, index - 1) }
            if (index + 1 < count) button(body, "下一页") { pdfPage(file, index + 1) }
            button(body, "用其他应用打开") { external(file) }
        }
    }

    private fun external(file: File) {
        val uri = FileProvider.getUriForFile(this, "${packageName}.files", file)
        val type =
            MimeTypeMap.getSingleton().getMimeTypeFromExtension(file.extension.lowercase())
                ?: "application/octet-stream"
        val intent =
            Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, type)
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        intent.clipData = ClipData.newRawUri(file.name, uri)
        try {
            startActivity(Intent.createChooser(intent, "读取文件"))
        } catch (e: Exception) {
            status.text = "未找到阅读器"
        }
    }

    @Deprecated("Activity navigation")
    override fun onBackPressed() {
        if (opened != null) {
            opened = null
            screen?.let { navigate(it, false) }
            return
        }
        if (trail.isNotEmpty()) {
            navigate(trail.removeLast(), false)
            return
        }
        super.onBackPressed()
    }

    override fun onDestroy() {
        cancel()
        worker.shutdownNow()
        super.onDestroy()
    }
}
