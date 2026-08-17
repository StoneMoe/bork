import { For, Show, createEffect, createSignal, onCleanup, onMount } from "solid-js";

export interface SelectOption {
  value: string;
  label: string;
}

interface SelectProps {
  id: string;
  value: string;
  options: readonly SelectOption[];
  labelledBy: string;
  disabled?: boolean;
  onChange: (value: string) => void;
}

const movementKeys: Partial<Record<string, number>> = { ArrowDown: 1, ArrowUp: -1 };
const edgeKeys: Partial<Record<string, "first" | "last">> = { Home: "first", End: "last" };

function isTypeaheadKey(event: KeyboardEvent) {
  return event.key.length === 1 && event.key !== " " && !event.altKey && !event.ctrlKey && !event.metaKey;
}

export default function Select(props: SelectProps) {
  const listboxID = `${props.id}-listbox`;
  const [open, setOpen] = createSignal(false);
  const [openAbove, setOpenAbove] = createSignal(false);
  // DOM focus stays on the trigger while this value moves the visual and screen-reader focus.
  const [activeValue, setActiveValue] = createSignal("");
  let root: HTMLDivElement | undefined;
  let searchText = "";
  let searchTimer: number | undefined;

  const selectedIndex = () => {
    const index = props.options.findIndex((option) => option.value === props.value);
    return index < 0 ? 0 : index;
  };
  const activeIndex = () => {
    const index = props.options.findIndex((option) => option.value === activeValue());
    return index < 0 ? selectedIndex() : index;
  };
  const optionID = (index: number) => `${listboxID}-option-${index}`;

  function focusOption(index: number) {
    const bounded = Math.max(0, Math.min(index, props.options.length - 1));
    setActiveValue(props.options[bounded].value);
    queueMicrotask(() => document.getElementById(optionID(bounded))?.scrollIntoView({ block: "nearest" }));
  }

  function showOptions(index = selectedIndex()) {
    if (props.disabled) return;
    const bounds = root?.getBoundingClientRect();
    const menuHeight = Math.min(240, props.options.length * 34 + 10, window.innerHeight * 0.4);
    setOpenAbove(Boolean(bounds && window.innerHeight - bounds.bottom < menuHeight && bounds.top > window.innerHeight - bounds.bottom));
    setOpen(true);
    focusOption(index);
  }

  function chooseActive() {
    const option = props.options[activeIndex()];
    setOpen(false);
    if (option.value !== props.value) props.onChange(option.value);
  }

  function handleCloseKey(event: KeyboardEvent) {
    // Settings uses Escape to close the drawer, so consume it only while this list is open.
    if (event.key === "Escape" && open()) {
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      return true;
    }
    if (event.key !== "Tab" || !open()) return false;
    setOpen(false);
    return true;
  }

  function findOption(query: string) {
    const start = query.length === 1 ? activeIndex() + 1 : 0;
    for (let offset = 0; offset < props.options.length; offset += 1) {
      const index = (start + offset) % props.options.length;
      if (props.options[index].label.toLocaleLowerCase().startsWith(query)) return index;
    }
    return -1;
  }

  function handleTypeahead(event: KeyboardEvent) {
    if (!isTypeaheadKey(event)) return false;
    event.preventDefault();
    window.clearTimeout(searchTimer);
    const character = event.key.toLocaleLowerCase();
    const query = searchText + character;
    let index = findOption(query);
    searchText = query;
    if (index < 0) {
      searchText = character;
      index = findOption(character);
    }
    searchTimer = window.setTimeout(() => { searchText = ""; }, 700);
    if (index < 0) return true;
    if (open()) focusOption(index);
    else showOptions(index);
    return true;
  }

  function handleNavigationKey(event: KeyboardEvent) {
    const movement = movementKeys[event.key];
    if (movement) {
      event.preventDefault();
      if (open()) focusOption(activeIndex() + movement);
      else showOptions();
      return true;
    }
    const edge = edgeKeys[event.key];
    if (!edge) return false;
    event.preventDefault();
    const index = edge === "first" ? 0 : props.options.length - 1;
    if (open()) focusOption(index);
    else showOptions(index);
    return true;
  }

  function handleActivationKey(event: KeyboardEvent) {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    if (open()) chooseActive();
    else showOptions();
  }

  function handleKeyDown(event: KeyboardEvent) {
    if (handleCloseKey(event)) return;
    if (handleNavigationKey(event)) return;
    if (handleTypeahead(event)) return;
    handleActivationKey(event);
  }

  function handleOutsidePointer(event: PointerEvent) {
    if (open() && !root?.contains(event.target as Node)) setOpen(false);
  }

  createEffect(() => {
    if (props.disabled) setOpen(false);
    if (!open() || !props.options.some((option) => option.value === activeValue())) {
      setActiveValue(props.options[selectedIndex()].value);
    }
  });
  onMount(() => document.addEventListener("pointerdown", handleOutsidePointer));
  onCleanup(() => {
    document.removeEventListener("pointerdown", handleOutsidePointer);
    window.clearTimeout(searchTimer);
  });

  return (
    <div ref={root} class="select" classList={{ open: open(), above: openAbove() }}>
      <button
        id={props.id}
        class="select-trigger"
        type="button"
        role="combobox"
        aria-labelledby={props.labelledBy}
        aria-expanded={open()}
        aria-controls={open() ? listboxID : undefined}
        aria-activedescendant={open() ? optionID(activeIndex()) : undefined}
        disabled={props.disabled}
        onClick={() => open() ? setOpen(false) : showOptions()}
        onKeyDown={handleKeyDown}
      >
        <span>{props.options[selectedIndex()].label}</span>
      </button>
      <Show when={open()}>
        <div id={listboxID} class="select-listbox" role="listbox" aria-labelledby={props.labelledBy}>
          <For each={props.options}>{(option, index) => (
            <div
              id={optionID(index())}
              class="select-option"
              classList={{ active: option.value === activeValue() }}
              role="option"
              aria-selected={option.value === activeValue()}
              onMouseDown={(event) => event.preventDefault()}
              onPointerEnter={() => setActiveValue(option.value)}
              onClick={() => {
                setActiveValue(option.value);
                chooseActive();
              }}
            >{option.label}</div>
          )}</For>
        </div>
      </Show>
    </div>
  );
}
