// Package source provides the SFTP file source for xpro-oss (direct, or
// tunneled through SAP BTP Connectivity when configured). A source is a plain
// io/fs.FS, so it interoperates with fs.ReadFile, fs.ReadDir and
// testing/fstest; local directories need no wrapper at all, since os.DirFS
// already is one. See sftp.go.
//
// A source that holds a session (like SFTP) is connected and closed by
// whoever creates it — never by the code that merely reads from it.
package source
