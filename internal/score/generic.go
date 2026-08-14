// The generic reference base (from techcrawl-go): covers vocabulary the
// upload corpus may miss (security, kernel, networking, databases...).
package score

// GenericExemplars returns the base reference paragraphs.
func GenericExemplars() []string { return EXEMPLARS }

var EXEMPLARS = []string{
	"A tutorial on Python programming: variables, control flow, functions, classes, generators, decorators and async. Learn how to structure a module, write tests with pytest, and profile your code.",
	"Understanding the Linux kernel: process scheduling, virtual memory, page tables, syscalls, interrupts, eBPF tracing, and how to read kernel source and write device drivers.",
	"Algorithm design and analysis: complexity bounds, divide and conquer, dynamic programming, graph algorithms, sorting, hashing, data structures, and how to prove correctness of an implementation.",
	"Computer networking from the ground up: TCP/IP, DNS, TLS handshake, HTTP/2, QUIC, sockets, packet capture with tcpdump, congestion control and debugging latency with curl.",
	"Practical security engineering: buffer overflows, shellcode, reverse engineering binaries with gdb, exploit development, fuzzing, cryptography primitives, and hardening a server configuration.",
	"Database internals and SQL: query planning, indexes and B-trees, transactions and isolation levels, joins, Postgres configuration, sharding, and profiling slow queries with EXPLAIN.",
	"Language design and semantics: static versus dynamic typing, ownership and lifetimes, pattern matching, algebraic data types, closures, and how the runtime executes a compiled program.",
	"DevOps and tooling: Docker images, Kubernetes pods and services, CI/CD pipelines, Terraform, monitoring with Prometheus and Grafana, logging and observability, and shell scripting with awk and jq.",
	"Compilers and programming language implementation: lexers, parsers, ASTs, type inference, code generation, garbage collection, and how type systems and subtyping guarantee memory safety across languages.",
	"Concurrency and performance: threads, goroutines, actors, locks versus lock-free structures, cache locality, profiling with perf and flame graphs, SIMD vectorization, and reducing tail latency.",
}
