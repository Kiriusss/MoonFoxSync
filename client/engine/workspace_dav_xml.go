package engine

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

var archiveDAVProps = []string{"displayname", "resourcetype", "getcontentlength", "getcontenttype", "getetag"}

// Stick to a small read-only DAV surface. A root directory listing deliberately
// omits the per-file capability directories even for Depth: 1 and allprop.
func (d *workspaceDAV) properties(w http.ResponseWriter, r *http.Request, f davFile, dir, root bool) {
	depth := r.Header.Get("Depth")
	if depth != "" && depth != "0" && depth != "1" {
		http.Error(w, "finite depth required", 403)
		return
	}
	var req struct {
		XMLName xml.Name  `xml:"propfind"`
		Names   *struct{} `xml:"propname"`
		Prop    *struct {
			Items []struct{ XMLName xml.Name } `xml:",any"`
		} `xml:"prop"`
	}
	if r.ContentLength != 0 {
		decoder := xml.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid properties", 400)
			return
		}
	}
	var requested []xml.Name
	if req.Prop == nil {
		for _, prop := range archiveDAVProps {
			requested = append(requested, xml.Name{Space: "DAV:", Local: prop})
		}
	} else {
		for _, p := range req.Prop.Items {
			requested = append(requested, p.XMLName)
		}
	}
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><multistatus xmlns="DAV:">`)
	enc := xml.NewEncoder(&out)
	write := func(elem, content string) {
		_ = enc.EncodeElement(content, xml.StartElement{Name: xml.Name{Local: elem}})
	}
	open := func(name xml.Name) xml.EndElement {
		el := xml.StartElement{Name: name}
		_ = enc.EncodeToken(el)
		return el.End()
	}
	local := func(s string) xml.Name { return xml.Name{Local: s} }
	row := func(p, display string, isDir bool) {
		resp := open(local("response"))
		write("href", (&url.URL{Path: p}).EscapedPath())
		var unsupported []xml.Name
		status := open(local("propstat"))
		props := open(local("prop"))
		for _, n := range requested {
			switch {
			case n.Space != "DAV:":
				unsupported = append(unsupported, n)
			case n.Local == "displayname":
				if req.Names != nil {
					write(n.Local, "")
				} else {
					write(n.Local, display)
				}
			case n.Local == "resourcetype":
				e := open(local(n.Local))
				if isDir && req.Names == nil {
					write("collection", "")
				}
				_ = enc.EncodeToken(e)
			case n.Local == "getcontentlength" && !isDir:
				if req.Names != nil {
					write(n.Local, "")
				} else {
					write(n.Local, strconv.FormatInt(f.size, 10))
				}
			case n.Local == "getcontenttype" && !isDir:
				if req.Names != nil {
					write(n.Local, "")
				} else {
					write(n.Local, "application/octet-stream")
				}
			case n.Local == "getetag" && !isDir:
				if req.Names != nil {
					write(n.Local, "")
				} else {
					write(n.Local, strconv.Quote(f.sha))
				}
			default:
				unsupported = append(unsupported, n)
			}
		}
		_ = enc.EncodeToken(props)
		write("status", "HTTP/1.1 200 OK")
		_ = enc.EncodeToken(status)
		if len(unsupported) > 0 && req.Prop != nil {
			status = open(local("propstat"))
			props = open(local("prop"))
			for _, n := range unsupported {
				e := open(n)
				_ = enc.EncodeToken(e)
			}
			_ = enc.EncodeToken(props)
			write("status", "HTTP/1.1 404 Not Found")
			_ = enc.EncodeToken(status)
		}
		_ = enc.EncodeToken(resp)
	}
	location := r.URL.Path
	if dir {
		location = strings.TrimSuffix(location, "/") + "/"
	}
	label := path.Base(f.member)
	if dir {
		label = f.slot
	}
	if root {
		label = "心狐同步"
	}
	row(location, label, dir)
	if dir && !root && depth == "1" {
		row(location+path.Base(f.member), path.Base(f.member), false)
	}
	_ = enc.Flush()
	out.WriteString("</multistatus>")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = w.Write(out.Bytes())
}
