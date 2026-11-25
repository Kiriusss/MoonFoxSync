package local.kirius.xinhusync

import android.content.Context
import android.net.Uri
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.math.BigInteger
import java.net.URI
import java.security.KeyStore
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateFactory
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec
import javax.net.ssl.HttpsURLConnection
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import org.json.JSONObject

internal fun bounded(input: InputStream, limit: Int): ByteArray {
    val out = java.io.ByteArrayOutputStream()
    val buf = ByteArray(8192)
    while (true) {
        val n = input.read(buf)
        if (n < 0) return out.toByteArray()
        if (out.size() + n > limit) throw IOException("响应超过读取上限")
        out.write(buf, 0, n)
    }
}

internal class Credentials(ctx: Context) {
    private val pref = ctx.getSharedPreferences("reader", Context.MODE_PRIVATE)
    private val keyAlias = "xinhu-device-v1"
    val active
        get() = pref.getBoolean("active", false)

    val address
        get() = pref.getString("url", "")!!

    val pem
        get() = pref.getString("pem", "")!!

    val id
        get() = pref.getString("id", "")!!

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (ks.getKey(keyAlias, null) as SecretKey?)?.let {
            return it
        }
        return KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore").run {
            init(
                KeyGenParameterSpec.Builder(
                        keyAlias,
                        KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
                    )
                    .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                    .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                    .build()
            )
            generateKey()
        }
    }

    fun initialize(url: String, cert: String) {
        if (id.isNotEmpty() && address == url && pem == cert) return
        val rng = SecureRandom()
        val device = ByteArray(16).also(rng::nextBytes)
        var now = System.currentTimeMillis()
        for (i in 5 downTo 0) {
            device[i] = now.toByte()
            now = now ushr 8
        }
        var x = BigInteger(1, device)
        val chars = CharArray(26)
        for (i in chars.indices.reversed()) {
            chars[i] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[x.and(BigInteger.valueOf(31)).toInt()]
            x = x.shiftRight(5)
        }
        val token = ByteArray(32).also(rng::nextBytes)
        val cipher =
            Cipher.getInstance("AES/GCM/NoPadding").apply { init(Cipher.ENCRYPT_MODE, key()) }
        val encrypted = cipher.doFinal(token)
        check(
            pref
                .edit()
                .clear()
                .putString("url", url)
                .putString("pem", cert)
                .putString("id", String(chars))
                .putString("iv", Base64.encodeToString(cipher.iv, Base64.NO_WRAP))
                .putString("secret", Base64.encodeToString(encrypted, Base64.NO_WRAP))
                .commit()
        )
    }

    fun token(): String {
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(
            Cipher.DECRYPT_MODE,
            key(),
            GCMParameterSpec(128, Base64.decode(pref.getString("iv", ""), Base64.NO_WRAP)),
        )
        val token = cipher.doFinal(Base64.decode(pref.getString("secret", ""), Base64.NO_WRAP))
        return Base64.encodeToString(token, Base64.URL_SAFE or Base64.NO_WRAP or Base64.NO_PADDING)
    }

    fun activate() {
        check(pref.edit().putBoolean("active", true).commit())
    }

    fun clear() {
        check(pref.edit().clear().commit())
        KeyStore.getInstance("AndroidKeyStore").apply {
            load(null)
            deleteEntry(keyAlias)
        }
    }
}

internal class HttpFailure(val status: Int, message: String) : IOException(message)

internal data class Folder(val unit: String, val snap: String?, val path: String = "")

internal data class Member(val folder: Folder, val path: String, val size: Long, val hash: String?)

internal class Nas(private val keys: Credentials) {
    @Volatile private var pending: HttpsURLConnection? = null

    fun cancel() {
        pending?.disconnect()
    }

    companion object {
        fun address(input: String): String {
            val u = URI(input.trim())
            require(
                u.scheme == "https" &&
                    u.host != null &&
                    u.userInfo == null &&
                    u.rawQuery == null &&
                    u.rawFragment == null &&
                    u.path in listOf("", "/", "/v1", "/v1/")
            ) {
                "请输入包含端口的 HTTPS 服务地址"
            }
            return "https://${u.rawAuthority}"
        }

        private val strongTag = Regex("\"([0-9a-f]{64})\"")

        private fun version(response: HttpsURLConnection, expected: String?): String {
            val digest =
                strongTag
                    .matchEntire(response.getHeaderField("ETag").orEmpty())
                    ?.groupValues
                    ?.get(1)
            require(digest != null && (expected == null || expected == digest)) { "文件版本已变化，请重新打开" }
            return digest
        }

        private fun unitPath(s: String): String {
            require(Regex("^[0-7][0-9A-HJKMNP-TV-Z]{25}$").matches(s)) { "无效的资料库 ID" }
            return "/units/$s"
        }
    }

    private var trustSource: String? = null
    private var ssl: SSLContext? = null

    private fun <T> request(
        path: String,
        params: Map<String, String> = emptyMap(),
        body: JSONObject? = null,
        range: String? = null,
        ifMatch: String? = null,
        sendToken: Boolean = true,
        fn: (HttpsURLConnection) -> T,
    ): T {
        val query = Uri.Builder()
        params.forEach { (k, v) -> query.appendQueryParameter(k, v) }
        val url = URI(keys.address + "/v1" + path + query.build().toString()).toURL()
        val c = url.openConnection() as HttpsURLConnection
        pending = c
        try {
            val pem = keys.pem
            if (pem.isNotBlank()) {
                if (trustSource != pem) {
                    val certs =
                        CertificateFactory.getInstance("X.509")
                            .generateCertificates(pem.byteInputStream())
                    require(certs.isNotEmpty()) { "证书为空" }
                    val anchors =
                        KeyStore.getInstance(KeyStore.getDefaultType()).apply {
                            load(null)
                            certs.forEachIndexed { i, cert -> setCertificateEntry("nas-$i", cert) }
                        }
                    val trust =
                        TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
                            .apply { init(anchors) }
                    ssl =
                        SSLContext.getInstance("TLS").apply {
                            init(null, trust.trustManagers, null)
                        }
                    trustSource = pem
                }
                c.sslSocketFactory = ssl!!.socketFactory
            }
            c.instanceFollowRedirects = false
            c.useCaches = false
            c.connectTimeout = 15000
            c.readTimeout = 30000
            c.setRequestProperty("Accept-Encoding", "identity")
            if (sendToken) c.setRequestProperty("Authorization", "Bearer ${keys.token()}")
            if (range != null) c.setRequestProperty("Range", range)
            if (ifMatch != null) c.setRequestProperty("If-Match", "\"$ifMatch\"")
            if (body != null) {
                c.requestMethod = "POST"
                c.doOutput = true
                c.setRequestProperty("Content-Type", "application/json")
                c.outputStream.use { it.write(body.toString().toByteArray()) }
            }
            val status = c.responseCode
            if (status !in 200..299) {
                val code =
                    runCatching {
                            c.errorStream?.use {
                                JSONObject(String(bounded(it, 16384))).optString("code")
                            }
                        }
                        .getOrNull()
                throw HttpFailure(status, "NAS 请求失败（$status ${code.orEmpty()}）")
            }
            return fn(c)
        } finally {
            c.disconnect()
            if (pending === c) pending = null
        }
    }

    private fun json(path: String, query: Map<String, String> = emptyMap()): JSONObject =
        request(path, query) { c ->
            c.inputStream.use { JSONObject(String(bounded(it, 4 shl 20))) }
        }

    fun pair(url: String, cert: String, code: String, deviceName: String) {
        keys.initialize(address(url), cert)
        val body =
            JSONObject()
                .put("code", code.trim())
                .put("deviceId", keys.id)
                .put("deviceName", deviceName)
                .put("token", keys.token())
        try {
            request("/pair", body = body, sendToken = false) { c ->
                val result = c.inputStream.use { JSONObject(String(bounded(it, 16384))) }
                require(result.getString("deviceId") == keys.id) { "NAS 返回了其他设备的配对结果" }
                if (result.getBoolean("canCreateUnits")) {
                    keys.clear()
                    throw IllegalArgumentException("请使用 NAS 签发的只读设备配对码")
                }
            }
        } catch (e: IOException) {
            // A successful enrollment can be followed by a dropped response.
            if (runCatching { units(null) }.isFailure) throw e
        }
        var cursor: String? = null
        do {
            val page = units(cursor)
            val items = page.getJSONArray("items")
            for (i in 0 until items.length()) {
                if (items.getJSONObject(i).getString("role") != "reader") {
                    keys.clear()
                    throw IllegalArgumentException("设备授权允许写入。请重新签发只读配对码")
                }
            }
            cursor = if (page.isNull("nextCursor")) null else page.getString("nextCursor")
        } while (cursor != null)
        keys.activate()
    }

    private fun paging(cursor: String?) = buildMap {
        put("limit", "100")
        if (cursor != null) put("cursor", cursor)
    }

    fun units(cursor: String?) = json("/units", paging(cursor))

    fun history(unit: String, cursor: String?) = json(unitPath(unit) + "/snapshots", paging(cursor))

    private fun query(folder: Folder, path: String) = buildMap {
        put("path", path)
        folder.snap?.let { put("snapshotId", it) }
    }

    fun list(folder: Folder, cursor: String?) =
        json(unitPath(folder.unit) + "/entries", query(folder, folder.path) + paging(cursor))

    fun preview(member: Member, offset: Long): Pair<ByteArray, String?> {
        if (member.size == 0L) return ByteArray(0) to member.hash
        val end = minOf(member.size - 1, offset + (64 shl 10) - 1)
        return request(
            unitPath(member.folder.unit) + "/content",
            query(member.folder, member.path),
            range = "bytes=$offset-$end",
            ifMatch = member.hash,
        ) { c ->
            require(
                c.responseCode == 206 &&
                    c.getHeaderField("Content-Range") == "bytes $offset-$end/${member.size}"
            ) {
                "版本或范围已变化，请重新打开"
            }
            val tag = version(c, member.hash)
            val b = c.inputStream.use { bounded(it, (end - offset + 1).toInt()) }
            require(b.size.toLong() == end - offset + 1) { "未读完整范围" }
            b to tag
        }
    }

    fun download(member: Member, dest: File) {
        try {
            require(member.size < dest.parentFile!!.usableSpace) { "手机可用空间不足" }
            request(
                unitPath(member.folder.unit) + "/content",
                query(member.folder, member.path),
                ifMatch = member.hash,
            ) { c ->
                require(c.responseCode == 200) { "未获得完整文件" }
                val etag = version(c, member.hash)
                val hash = MessageDigest.getInstance("SHA-256")
                var n = 0L
                c.inputStream.use { input ->
                    dest.outputStream().use { output ->
                        val buf = ByteArray(65536)
                        while (true) {
                            if (Thread.currentThread().isInterrupted) throw IOException("已取消")
                            val len = input.read(buf)
                            if (len < 0) break
                            n += len
                            require(n <= member.size) { "文件长度发生变化" }
                            hash.update(buf, 0, len)
                            output.write(buf, 0, len)
                        }
                    }
                }
                require(
                    n == member.size && hash.digest().joinToString("") { "%02x".format(it) } == etag
                ) {
                    "文件未通过校验"
                }
            }
        } catch (e: Exception) {
            dest.delete()
            throw e
        }
    }
}
