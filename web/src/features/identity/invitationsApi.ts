import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  AcceptInvitationRequest,
  CreateInvitationRequest,
  CreateInvitationResult,
  Invitation,
  InvitationPreview,
  LoginResult,
} from "../../api/types";

const invitationsKey = ["invitations"] as const;

export function useInvitations() {
  return useQuery({ queryKey: invitationsKey, queryFn: ({ signal }) => api<Invitation[]>("GET", "/api/invitations", undefined, signal) });
}

export function useCreateInvitation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: CreateInvitationRequest) => api<CreateInvitationResult>("POST", "/api/invitations", req),
    gcTime: 0, // the response carries the one-time token
    onSuccess: () => qc.invalidateQueries({ queryKey: invitationsKey }),
  });
}

export function useRevokeInvitation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<void>("POST", `/api/invitations/${encodeURIComponent(id)}/revoke`),
    onSuccess: () => qc.invalidateQueries({ queryKey: invitationsKey }),
  });
}

/** The token goes in the POST body, never in a URL the server sees. Not cached: it is checked once per page. */
export function useInvitationPreview(token: string) {
  return useQuery({
    queryKey: ["invitation-preview"],
    enabled: token !== "",
    gcTime: 0,
    retry: false,
    // Checked once: refetching would re-send the token, spend the shared rate limit and, after acceptance,
    // turn into "already used".
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    queryFn: ({ signal }) => api<InvitationPreview>("POST", "/api/invitations/preview", { token }, signal),
  });
}

export function useAcceptInvitation() {
  return useMutation({
    mutationFn: (req: AcceptInvitationRequest) => api<LoginResult>("POST", "/api/invitations/accept", req),
    gcTime: 0, // the variables hold the token and the new password
  });
}

/** The link the Owner hands over. The token sits in the fragment, which browsers never send to the server. */
export function invitationLink(token: string, origin = window.location.origin): string {
  return `${origin}/invite#token=${encodeURIComponent(token)}`;
}
