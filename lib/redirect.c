// redirect.c — LD_PRELOAD library that forces HTTPS traffic through an HTTP CONNECT proxy.
//
// Intercepts connect() on port 443 and tunnels through a local proxy.
// The proxy does TLS MITM, enabling inspection of WebSocket/HTTP traffic
// from applications that ignore HTTPS_PROXY (e.g. tokio-tungstenite).
//
// Build: gcc -shared -fPIC -o redirect.so redirect.c -ldl -lpthread
// Usage: LD_PRELOAD=./redirect.so REDIRECT_PROXY_PORT=8765 codex ...

#define _GNU_SOURCE
#include <dlfcn.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <string.h>
#include <unistd.h>
#include <stdlib.h>
#include <stdio.h>
#include <pthread.h>
#include <errno.h>
#include <fcntl.h>

#define MAX_MAPPINGS 512

static struct {
    char ip[INET6_ADDRSTRLEN];
    char host[256];
} dns_map[MAX_MAPPINGS];
static int dns_map_count = 0;
static pthread_mutex_t dns_mutex = PTHREAD_MUTEX_INITIALIZER;

static int proxy_port = 0;
static int init_done = 0;
static int verbose = 0;

static void ensure_init(void) {
    if (init_done) return;
    init_done = 1;
    char *p = getenv("REDIRECT_PROXY_PORT");
    if (p) proxy_port = atoi(p);
    if (getenv("REDIRECT_VERBOSE")) verbose = 1;
}

static void record_dns(const char *host, const struct addrinfo *res) {
    if (!host || !res) return;
    pthread_mutex_lock(&dns_mutex);
    for (const struct addrinfo *ai = res; ai && dns_map_count < MAX_MAPPINGS; ai = ai->ai_next) {
        char ipstr[INET6_ADDRSTRLEN] = {0};
        void *addr = NULL;
        if (ai->ai_family == AF_INET) {
            addr = &((struct sockaddr_in *)ai->ai_addr)->sin_addr;
        } else if (ai->ai_family == AF_INET6) {
            addr = &((struct sockaddr_in6 *)ai->ai_addr)->sin6_addr;
        }
        if (!addr) continue;
        inet_ntop(ai->ai_family, addr, ipstr, sizeof(ipstr));

        int found = 0;
        for (int i = 0; i < dns_map_count; i++) {
            if (strcmp(dns_map[i].ip, ipstr) == 0) {
                // Update hostname if we have a new one
                if (strcmp(dns_map[i].host, host) != 0) {
                    strncpy(dns_map[i].host, host, sizeof(dns_map[i].host) - 1);
                }
                found = 1;
                break;
            }
        }
        if (!found) {
            strncpy(dns_map[dns_map_count].ip, ipstr, sizeof(dns_map[dns_map_count].ip) - 1);
            strncpy(dns_map[dns_map_count].host, host, sizeof(dns_map[dns_map_count].host) - 1);
            dns_map_count++;
        }
    }
    pthread_mutex_unlock(&dns_mutex);
}

static const char *lookup_host(const char *ip) {
    pthread_mutex_lock(&dns_mutex);
    for (int i = 0; i < dns_map_count; i++) {
        if (strcmp(dns_map[i].ip, ip) == 0) {
            const char *h = dns_map[i].host;
            pthread_mutex_unlock(&dns_mutex);
            return h;
        }
    }
    pthread_mutex_unlock(&dns_mutex);
    return NULL;
}

typedef int (*orig_connect_t)(int, const struct sockaddr *, socklen_t);
typedef int (*orig_getaddrinfo_t)(const char *, const char *, const struct addrinfo *, struct addrinfo **);

int getaddrinfo(const char *node, const char *service,
                const struct addrinfo *hints, struct addrinfo **res) {
    orig_getaddrinfo_t orig = (orig_getaddrinfo_t)dlsym(RTLD_NEXT, "getaddrinfo");
    int ret = orig(node, service, hints, res);
    if (ret == 0 && node && res) {
        record_dns(node, *res);
    }
    return ret;
}

int connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    ensure_init();

    orig_connect_t orig_connect = (orig_connect_t)dlsym(RTLD_NEXT, "connect");

    if (proxy_port <= 0) {
        return orig_connect(sockfd, addr, addrlen);
    }

    // Only intercept IPv4 and IPv6
    if (addr->sa_family != AF_INET && addr->sa_family != AF_INET6) {
        return orig_connect(sockfd, addr, addrlen);
    }

    int dest_port = 0;
    char dest_ip[INET6_ADDRSTRLEN] = {0};

    if (addr->sa_family == AF_INET) {
        struct sockaddr_in *sin = (struct sockaddr_in *)addr;
        dest_port = ntohs(sin->sin_port);
        inet_ntop(AF_INET, &sin->sin_addr, dest_ip, sizeof(dest_ip));
    } else {
        struct sockaddr_in6 *sin6 = (struct sockaddr_in6 *)addr;
        dest_port = ntohs(sin6->sin6_port);
        inet_ntop(AF_INET6, &sin6->sin6_addr, dest_ip, sizeof(dest_ip));
    }

    // Only intercept port 443 (HTTPS)
    if (dest_port != 443) {
        return orig_connect(sockfd, addr, addrlen);
    }

    // Skip connections that are already going to localhost
    if (strncmp(dest_ip, "127.", 4) == 0 || strcmp(dest_ip, "::1") == 0 ||
        strcmp(dest_ip, "0.0.0.0") == 0) {
        return orig_connect(sockfd, addr, addrlen);
    }

    // Look up hostname from DNS map
    const char *hostname = lookup_host(dest_ip);
    if (!hostname) hostname = dest_ip;

    if (verbose) {
        fprintf(stderr, "[redirect] %s (%s):%d -> proxy :%d (sockfd=%d)\n", hostname, dest_ip, dest_port, proxy_port, sockfd);
    }

    // Save and clear non-blocking flag for synchronous CONNECT handshake
    int orig_flags = fcntl(sockfd, F_GETFL, 0);
    if (orig_flags & O_NONBLOCK) {
        fcntl(sockfd, F_SETFL, orig_flags & ~O_NONBLOCK);
    }

    // Connect to local proxy instead
    struct sockaddr_in proxy_addr;
    memset(&proxy_addr, 0, sizeof(proxy_addr));
    proxy_addr.sin_family = AF_INET;
    proxy_addr.sin_addr.s_addr = inet_addr("127.0.0.1");
    proxy_addr.sin_port = htons(proxy_port);

    int ret = orig_connect(sockfd, (struct sockaddr *)&proxy_addr, sizeof(proxy_addr));
    if (ret < 0) {
        if (verbose) fprintf(stderr, "[redirect] orig_connect failed: %s\n", strerror(errno));
        if (orig_flags & O_NONBLOCK) fcntl(sockfd, F_SETFL, orig_flags);
        return -1;
    }
    if (verbose) fprintf(stderr, "[redirect] connected to proxy\n");

    // Send CONNECT request to establish tunnel
    char connect_req[512];
    int reqlen = snprintf(connect_req, sizeof(connect_req),
        "CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n",
        hostname, dest_port, hostname, dest_port);

    ssize_t wret = write(sockfd, connect_req, reqlen);
    if (wret != reqlen) {
        if (verbose) fprintf(stderr, "[redirect] write failed: %zd/%d (%s)\n", wret, reqlen, strerror(errno));
        if (orig_flags & O_NONBLOCK) fcntl(sockfd, F_SETFL, orig_flags);
        return -1;
    }
    if (verbose) fprintf(stderr, "[redirect] sent CONNECT (%d bytes)\n", reqlen);

    // Read proxy response until \r\n\r\n
    char buf[512];
    int total = 0;
    while (total < (int)sizeof(buf) - 1) {
        int n = read(sockfd, buf + total, 1);
        if (n <= 0) {
            if (verbose) fprintf(stderr, "[redirect] read response failed at %d bytes\n", total);
            if (orig_flags & O_NONBLOCK) fcntl(sockfd, F_SETFL, orig_flags);
            return -1;
        }
        total += n;
        buf[total] = '\0';
        if (total >= 4 && memcmp(buf + total - 4, "\r\n\r\n", 4) == 0) break;
    }

    if (verbose) fprintf(stderr, "[redirect] proxy response: %.*s\n", total < 60 ? total : 60, buf);

    // Check for 200 Connection Established
    if (!strstr(buf, "200")) {
        if (verbose) fprintf(stderr, "[redirect] response missing 200\n");
        if (orig_flags & O_NONBLOCK) fcntl(sockfd, F_SETFL, orig_flags);
        return -1;
    }

    // Restore original flags (non-blocking if it was)
    if (orig_flags & O_NONBLOCK) {
        fcntl(sockfd, F_SETFL, orig_flags);
    }

    if (verbose) fprintf(stderr, "[redirect] tunnel established for %s:%d\n", hostname, dest_port);
    return 0;
}
