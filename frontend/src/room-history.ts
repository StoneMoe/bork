export interface RoomHistoryEntry {
  name: string;
  invite: string;
  visitedAt: number;
}

export const roomHistoryStorageKey = "bork.roomHistory";
const roomHistoryLimit = 6;

export function parseRoomHistory(value: string | null): RoomHistoryEntry[] {
  try {
    const entries: unknown = JSON.parse(value || "[]");
    if (!Array.isArray(entries)) return [];
    return entries.filter((entry): entry is RoomHistoryEntry => {
      if (!entry || typeof entry !== "object") return false;
      const room = entry as Partial<RoomHistoryEntry>;
      return typeof room.name === "string" && room.name.length > 0 && room.name.length <= 256
        && typeof room.invite === "string" && room.invite.length > 0 && room.invite.length <= 512
        && typeof room.visitedAt === "number" && Number.isFinite(room.visitedAt) && room.visitedAt > 0;
    }).slice(0, roomHistoryLimit);
  } catch {
    return [];
  }
}

export function withRecentRoom(history: readonly RoomHistoryEntry[], room: RoomHistoryEntry): RoomHistoryEntry[] {
  return [room, ...history.filter((entry) => entry.invite !== room.invite)].slice(0, roomHistoryLimit);
}
