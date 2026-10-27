# Third-party notices

The project is MIT licensed except for files carrying their own license notices.
Kubebuilder generated source files with Apache License 2.0 headers retain those
headers and remain subject to Apache License 2.0; the full text is in
[LICENSES/Apache-2.0.txt](LICENSES/Apache-2.0.txt). Generation with the current
`hack/boilerplate.go.txt` preserves those notices. The Go modules listed in
`go.mod` and `go.sum` retain their upstream licenses; they are not relicensed
by this project. The controller runs the official Forgejo image as a separate
workload; Forgejo itself is not included in this source repository or manager
image. `internal/rclone/crypt.go` implements the documented rclone crypt file
format, using `golang.org/x/crypto` primitives; its code is maintained here.
