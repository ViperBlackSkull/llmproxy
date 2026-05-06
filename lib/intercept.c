// intercept.c — ptrace-based connect() interceptor for static binaries.
//
// Redirects outbound connections to port 443 through a local MITM proxy.
// Works with statically linked binaries (like Codex's MUSL build) where
// LD_PRELOAD has no effect.
//
// Build: gcc -o intercept intercept.c
// Usage: ./intercept <proxy_port> <command> [args...]
//
// The child process runs under ptrace. When it calls connect() with
// port 443, the destination address is rewritten to 127.0.0.1:proxy_port.

#define _GNU_SOURCE
#include <sys/ptrace.h>
#include <sys/user.h>
#include <sys/wait.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <errno.h>
#include <fcntl.h>

// x86_64 syscall numbers
#define SYS_CONNECT 42
#define SYS_WRITE   1
#define SYS_READ    0
#define SYS_CLOSE   3

static int proxy_port = 0;
static int verbose = 0;

// Read null-terminated string from child process memory
static int read_child_string(pid_t child, unsigned long addr, char *buf, size_t maxlen) {
    size_t i = 0;
    while (i < maxlen - 1) {
        long word = ptrace(PTRACE_PEEKDATA, child, addr + i, NULL);
        if (word == -1 && errno != 0) return -1;
        memcpy(buf + i, &word, sizeof(long));
        for (size_t j = 0; j < sizeof(long) && i < maxlen - 1; j++, i++) {
            if (buf[i] == '\0') return 0;
        }
    }
    buf[maxlen - 1] = '\0';
    return 0;
}

// Write data to child process memory
static int write_child_mem(pid_t child, unsigned long addr, const void *data, size_t len) {
    for (size_t i = 0; i < len; i += sizeof(long)) {
        long word = 0;
        size_t remaining = len - i;
        if (remaining >= sizeof(long)) {
            memcpy(&word, (const char *)data + i, sizeof(long));
        } else {
            // Partial write: read existing word first, then modify
            long existing = ptrace(PTRACE_PEEKDATA, child, addr + i, NULL);
            if (existing == -1 && errno != 0) return -1;
            memcpy(&word, (const char *)data + i, remaining);
            // Keep the upper bytes from existing
            unsigned char *dst = (unsigned char *)&word + remaining;
            const unsigned char *src = (const unsigned char *)&existing + remaining;
            for (size_t j = 0; j < sizeof(long) - remaining; j++) dst[j] = src[j];
        }
        if (ptrace(PTRACE_POKEDATA, child, addr + i, word) == -1) return -1;
    }
    return 0;
}

// Get orig_rax (syscall number) from child registers
static long get_syscall_nr(pid_t child) {
    struct user_regs_struct regs;
    if (ptrace(PTRACE_GETREGS, child, NULL, &regs) == -1) return -1;
    return regs.orig_rax;
}

// Get syscall arguments
static void get_syscall_args(pid_t child, long *args, int nargs) {
    struct user_regs_struct regs;
    ptrace(PTRACE_GETREGS, child, NULL, &regs);
    if (nargs >= 1) args[0] = regs.rdi; // fd
    if (nargs >= 2) args[1] = regs.rsi; // addr ptr
    if (nargs >= 3) args[2] = regs.rdx; // addrlen
}

// Set return value of syscall
static void set_syscall_ret(pid_t child, long ret) {
    struct user_regs_struct regs;
    ptrace(PTRACE_GETREGS, child, NULL, &regs);
    regs.rax = ret;
    ptrace(PTRACE_SETREGS, child, NULL, &regs);
}

// Execute a write() syscall in the child process
static int child_write(pid_t child, int fd, const void *data, size_t len) {
    // We need to put data somewhere in the child's memory.
    // Use the stack area (RSP points to it).
    struct user_regs_struct regs;
    ptrace(PTRACE_GETREGS, child, NULL, &regs);

    // Use area below the stack pointer
    unsigned long buf_addr = regs.rsp - 512;
    if (write_child_mem(child, buf_addr, data, len) == -1) return -1;

    // Save current registers
    struct user_regs_struct saved = regs;

    // Set up write(fd, buf_addr, len) syscall
    regs.rax = SYS_WRITE;
    regs.rdi = fd;
    regs.rsi = buf_addr;
    regs.rdx = len;
    ptrace(PTRACE_SETREGS, child, NULL, &regs);

    // Execute syscall: single-step to enter, then continue to exit
    ptrace(PTRACE_SYSCALL, child, NULL, NULL);
    waitpid(child, NULL, 0);
    ptrace(PTRACE_SYSCALL, child, NULL, NULL);
    waitpid(child, NULL, 0);

    // Get return value
    struct user_regs_struct after;
    ptrace(PTRACE_GETREGS, child, NULL, &after);
    long written = (long)after.rax;

    // Restore registers
    ptrace(PTRACE_SETREGS, child, NULL, &saved);
    return (int)written;
}

// Execute a read() syscall in the child process
static int child_read(pid_t child, int fd, void *data, size_t maxlen) {
    struct user_regs_struct regs;
    ptrace(PTRACE_GETREGS, child, NULL, &regs);

    unsigned long buf_addr = regs.rsp - 1024;

    struct user_regs_struct saved = regs;
    regs.rax = SYS_READ;
    regs.rdi = fd;
    regs.rsi = buf_addr;
    regs.rdx = maxlen;
    ptrace(PTRACE_SETREGS, child, NULL, &regs);

    ptrace(PTRACE_SYSCALL, child, NULL, NULL);
    waitpid(child, NULL, 0);
    ptrace(PTRACE_SYSCALL, child, NULL, NULL);
    waitpid(child, NULL, 0);

    struct user_regs_struct after;
    ptrace(PTRACE_GETREGS, child, NULL, &after);
    long nread = (long)after.rax;

    ptrace(PTRACE_SETREGS, child, NULL, &saved);

    if (nread > 0) {
        // Read data from child memory
        for (long i = 0; i < nread; i += sizeof(long)) {
            long word = ptrace(PTRACE_PEEKDATA, child, buf_addr + i, NULL);
            size_t chunk = (size_t)(nread - i);
            if (chunk > sizeof(long)) chunk = sizeof(long);
            memcpy((char *)data + i, &word, chunk);
        }
    }
    return (int)nread;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "Usage: %s <proxy_port> <command> [args...]\n", argv[0]);
        return 1;
    }

    proxy_port = atoi(argv[1]);
    if (proxy_port <= 0) {
        fprintf(stderr, "Invalid proxy port: %s\n", argv[1]);
        return 1;
    }

    if (getenv("INTERCEPT_VERBOSE")) verbose = 1;

    pid_t child = fork();
    if (child == -1) {
        perror("fork");
        return 1;
    }

    if (child == 0) {
        // Child: request tracing and exec
        ptrace(PTRACE_TRACEME, 0, NULL, NULL);
        raise(SIGSTOP);
        execvp(argv[2], &argv[2]);
        perror("execvp");
        return 1;
    }

    // Parent: trace child
    int status;
    waitpid(child, &status, 0); // Wait for initial SIGSTOP

    // Set ptrace options: trace clone events so we can follow child threads
    ptrace(PTRACE_SETOPTIONS, child, NULL,
           (void *)(PTRACE_O_TRACESYSGOOD | PTRACE_O_TRACECLONE |
                    PTRACE_O_TRACEFORK | PTRACE_O_TRACEVFORK));

    // Continue past the SIGSTOP
    ptrace(PTRACE_SYSCALL, child, NULL, NULL);

    while (1) {
        pid_t waited = waitpid(-1, &status, __WALL);
        if (waited == -1) {
            if (errno == ECHILD) break; // No more children
            perror("waitpid");
            break;
        }

        if (WIFEXITED(status) || WIFSIGNALED(status)) {
            if (waited == child) break; // Main process exited
            continue;
        }

        if (!WIFSTOPPED(status)) continue;

        int sig = WSTOPSIG(status);
        int event = (status >> 16) & 0xFF;

        // Handle new child processes (fork/clone)
        if (event == PTRACE_EVENT_FORK || event == PTRACE_EVENT_VFORK ||
            event == PTRACE_EVENT_CLONE) {
            ptrace(PTRACE_SYSCALL, waited, NULL, NULL);
            continue;
        }

        // Only handle syscall-stops (bit 7 set in signal)
        if ((status >> 8) != (SIGTRAP | 0x80)) {
            // Real signal — deliver to child
            if (sig != SIGSTOP && sig != SIGTRAP) {
                ptrace(PTRACE_SYSCALL, waited, NULL, (void *)(long)sig);
            } else {
                ptrace(PTRACE_SYSCALL, waited, NULL, NULL);
            }
            continue;
        }

        long nr = get_syscall_nr(waited);
        if (nr == -1) {
            ptrace(PTRACE_SYSCALL, waited, NULL, NULL);
            continue;
        }

        // We only care about connect() syscalls
        if (nr != SYS_CONNECT) {
            ptrace(PTRACE_SYSCALL, waited, NULL, NULL);
            continue;
        }

        // This is a connect() call — read arguments
        long args[3];
        get_syscall_args(waited, args, 3);
        int fd = (int)args[0];
        unsigned long addr_ptr = (unsigned long)args[1];
        // socklen_t addrlen = (socklen_t)args[2];

        // Read sockaddr from child memory
        struct sockaddr_storage ss;
        memset(&ss, 0, sizeof(ss));
        for (size_t i = 0; i < sizeof(struct sockaddr_in); i += sizeof(long)) {
            long word = ptrace(PTRACE_PEEKDATA, waited, addr_ptr + i, NULL);
            if (word == -1 && errno != 0) break;
            memcpy((char *)&ss + i, &word, sizeof(long));
        }

        if (ss.ss_family == AF_INET) {
            struct sockaddr_in *sin = (struct sockaddr_in *)&ss;
            int dest_port = ntohs(sin->sin_port);
            char dest_ip[INET_ADDRSTRLEN];
            inet_ntop(AF_INET, &sin->sin_addr, dest_ip, sizeof(dest_ip));

            // Only intercept port 443, skip localhost
            if (dest_port == 443 &&
                strncmp(dest_ip, "127.", 4) != 0 &&
                strcmp(dest_ip, "0.0.0.0") != 0) {

                if (verbose) {
                    fprintf(stderr, "[intercept] %s:%d -> 127.0.0.1:%d (pid=%d)\n",
                            dest_ip, dest_port, proxy_port, waited);
                }

                // Modify sockaddr to proxy address
                sin->sin_addr.s_addr = inet_addr("127.0.0.1");
                sin->sin_port = htons(proxy_port);

                // Write modified sockaddr back to child memory
                if (write_child_mem(waited, addr_ptr, &ss, sizeof(struct sockaddr_in)) == -1) {
                    if (verbose) fprintf(stderr, "[intercept] write_child_mem failed\n");
                }
            }
        }

        ptrace(PTRACE_SYSCALL, waited, NULL, NULL);
    }

    if (WIFEXITED(status)) return WEXITSTATUS(status);
    return 1;
}
