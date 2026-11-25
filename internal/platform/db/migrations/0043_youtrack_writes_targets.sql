-- YouTrack write-back order (KIN-117): each delivery looks for newer writes of the same issue or article section
-- (NewerWrites); these indexes keep that lookup from scanning the whole write history.
CREATE INDEX youtrack_writes_issue ON youtrack_writes(issue_id, kind);
CREATE INDEX youtrack_writes_target ON youtrack_writes(target) WHERE target IS NOT NULL;
