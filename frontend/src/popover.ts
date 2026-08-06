export const closePopoversEvent = "bork:close-popovers";

function supportsNativePopover() {
  try {
    return typeof HTMLElement !== "undefined"
      && typeof HTMLElement.prototype.showPopover === "function"
      && typeof HTMLElement.prototype.hidePopover === "function"
      && typeof CSS !== "undefined"
      && CSS.supports("selector(:popover-open)");
  } catch {
    return false;
  }
}

export const nativePopoverSupported = supportsNativePopover();

export function nativePopoverOpen(element?: HTMLElement) {
  return nativePopoverSupported && Boolean(element?.matches(":popover-open"));
}
