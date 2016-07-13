package routers

import (
	"net/http"

	"github.com/SKatiyar/qr"
)

func Qrcode(r *http.Request, w http.ResponseWriter) {
	text := r.FormValue("text")
	code, _ := qr.Encode(text, qr.M)
	w.Header().Set("Content-Type", "image/png")
	w.Write(code.PNG())
}
