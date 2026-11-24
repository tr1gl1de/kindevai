import { createContext, useCallback, useContext, useState, type ReactNode } from "react";

const NotifyContext = createContext<(message: string) => void>(() => {});

/** One polite live region for success messages ("Project created."). */
export function NotifyProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState("");
  const notify = useCallback((m: string) => setMessage(m), []);
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
