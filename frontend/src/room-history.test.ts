import { expect, it } from "vitest";
import { parseRoomHistory, withRecentRoom } from "./room-history";

it("loads safe recent rooms and moves a revisited room to the front", () => {
  const rooms = parseRoomHistory(JSON.stringify([
    { name: "Alpha", invite: "invite-a", visitedAt: 1 },
    { name: "Broken", invite: "", visitedAt: 2 },
    { name: "Bravo", invite: "invite-b", visitedAt: 3 },
  ]));

  expect(withRecentRoom(rooms, { name: "Alpha renamed", invite: "invite-a", visitedAt: 4 })).toEqual([
    { name: "Alpha renamed", invite: "invite-a", visitedAt: 4 },
    { name: "Bravo", invite: "invite-b", visitedAt: 3 },
  ]);
  expect(parseRoomHistory("not json")).toEqual([]);
});
