module nodemesh

// 1.20 y no una versión más nueva a propósito: es el último toolchain de Go
// que produce binarios que arrancan en Windows 8.1 (Go 1.21 subió el mínimo a
// Windows 10), y rigby —el HP Stream 7, 32-bit— corre 8.1. El código es solo
// stdlib clásica, así que no cuesta nada quedarse aquí.
go 1.20
