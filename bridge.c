#include "bridge.h"

#include <stdatomic.h>

static _Atomic(const cliproxy_host_api*) stored_host = NULL;

void store_host_api(const cliproxy_host_api* host) {
	atomic_store_explicit(&stored_host, host, memory_order_release);
}

int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	const cliproxy_host_api* host = atomic_load_explicit(&stored_host, memory_order_acquire);
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

void free_host_buffer(void* ptr, size_t len) {
	const cliproxy_host_api* host = atomic_load_explicit(&stored_host, memory_order_acquire);
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
