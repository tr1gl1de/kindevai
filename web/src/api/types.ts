// Shared API request/response shapes. Keep in sync with internal/server (one module, never duplicated).

export interface Health {
  status: "ok";
  version: string;
}

export interface ApiError {
  error: string;
  message: string;
}
