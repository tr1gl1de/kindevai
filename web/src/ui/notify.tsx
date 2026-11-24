import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";

const NotifyContext = createContext<(message: string) => void>(() => {});

const VISIBLE_MS = 6000;

/** One polite live region for success messages ("Project created."). Messages hide after a few seconds;
 *  repeating the same message re-announces it. */
export function NotifyProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState("");
  const timers = useRef<number[]>([]);

  const notify = useCallback((m: string) => {
    timers.current.forEach(clearTimeout);
    setMessage("");
    timers.current = [
      window.setTimeout(() => setMessage(m), 30), // empty first, so screen readers announce repeats
      window.setTimeout(() => setMessage(""), VISIBLE_MS),
    ];
  }, []);

  useEffect(() => () => timers.current.forEach(clearTimeout), []);

  return (
    <NotifyContext.Provider value={notify}>
      {children}
      <div role="status" className="notice" data-visible={message ? "true" : "false"}>
        {message}
      </div>
    </NotifyContext.Provider>
  );
}

export function useNotify() {
  return useContext(NotifyContext);
}
