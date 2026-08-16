//go:build darwin && !ios && cgo

#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>

extern void borkGlobalKeyEvent(uintptr_t callback_handle, int pressed);

typedef struct bork_key_listener {
    CFMachPortRef event_tap;
    CFRunLoopSourceRef source;
    CFRunLoopRef run_loop;
    uintptr_t callback_handle;
    uint16_t key_code;
    atomic_bool stopped;
} bork_key_listener;

static CGEventRef bork_key_event(
    CGEventTapProxy proxy,
    CGEventType type,
    CGEventRef event,
    void *user_info
) {
    (void)proxy;
    bork_key_listener *listener = user_info;
    if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
        // A key-up may have been lost while the tap was disabled. Release the
        // audio gate before trying to resume observation.
        borkGlobalKeyEvent(listener->callback_handle, 0);
        if (!atomic_load_explicit(&listener->stopped, memory_order_relaxed)) {
            CGEventTapEnable(listener->event_tap, true);
        }
        return event;
    }
    if (type != kCGEventKeyDown && type != kCGEventKeyUp) {
        return event;
    }

    int64_t key_code = CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
    if (key_code == listener->key_code) {
        borkGlobalKeyEvent(listener->callback_handle, type == kCGEventKeyDown);
    }
    // This is a passive tap: the game or foreground application still receives
    // the configured push-to-talk key.
    return event;
}

bork_key_listener *bork_key_listener_create(uint16_t key_code, uintptr_t callback_handle) {
    bork_key_listener *listener = calloc(1, sizeof(*listener));
    if (listener == NULL) {
        return NULL;
    }
    listener->key_code = key_code;
    listener->callback_handle = callback_handle;
    atomic_init(&listener->stopped, false);

    CGEventMask mask = CGEventMaskBit(kCGEventKeyDown) | CGEventMaskBit(kCGEventKeyUp);
    listener->event_tap = CGEventTapCreate(
        kCGSessionEventTap,
        kCGHeadInsertEventTap,
        kCGEventTapOptionListenOnly,
        mask,
        bork_key_event,
        listener
    );
    if (listener->event_tap == NULL) {
        free(listener);
        return NULL;
    }

    listener->source = CFMachPortCreateRunLoopSource(NULL, listener->event_tap, 0);
    listener->run_loop = CFRunLoopGetCurrent();
    if (listener->source == NULL || listener->run_loop == NULL) {
        if (listener->source != NULL) {
            CFRelease(listener->source);
        }
        CFRelease(listener->event_tap);
        free(listener);
        return NULL;
    }
    CFRetain(listener->run_loop);
    CFRunLoopAddSource(listener->run_loop, listener->source, kCFRunLoopCommonModes);
    CGEventTapEnable(listener->event_tap, true);
    return listener;
}

void bork_key_listener_run(bork_key_listener *listener) {
    // A short upper bound also covers the narrow race where Stop arrives just
    // before CFRunLoopRunInMode begins.
    while (!atomic_load_explicit(&listener->stopped, memory_order_acquire)) {
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.1, false);
    }
}

void bork_key_listener_stop(bork_key_listener *listener) {
    atomic_store_explicit(&listener->stopped, true, memory_order_release);
    CFRunLoopStop(listener->run_loop);
    CFRunLoopWakeUp(listener->run_loop);
}

void bork_key_listener_destroy(bork_key_listener *listener) {
    CGEventTapEnable(listener->event_tap, false);
    CFRunLoopRemoveSource(listener->run_loop, listener->source, kCFRunLoopCommonModes);
    CFMachPortInvalidate(listener->event_tap);
    CFRelease(listener->source);
    CFRelease(listener->event_tap);
    CFRelease(listener->run_loop);
    free(listener);
}
