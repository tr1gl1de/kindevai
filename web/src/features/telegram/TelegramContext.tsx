import { createContext, useContext } from "react";
import type { TelegramLaunch } from "./webapp";

/** Launch data when the app runs inside Telegram, null in a normal browser. */
export const TelegramContext = createContext<TelegramLaunch | null>(null);

export function useTelegramLaunch(): TelegramLaunch | null {
  return useContext(TelegramContext);
}
