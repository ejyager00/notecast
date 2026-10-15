;; Development environment for notecast.
;;
;; The Kokoro packages live in the personal channel (ejyager00 packages
;; speech). Until that channel commit is pulled, `just` passes
;; -L ~/.guix-channel/guix so they resolve from the checkout.
(specifications->manifest
 (list
  ;; server
  "go" "gcc-toolchain" "sqlite"   ; gcc: cgo for go test -race; staticcheck is a go.mod tool
  ;; worker runtime
  "python" "python-httpx" "python-pillow" "python-numpy" "python-soundfile"
  "python-mutagen" "python-kokoro-onnx" "libheif" "ffmpeg" "llama-cpp"
  ;; tooling
  "just" "ruff" "python-pytest" "coreutils" "curl" "git"))
