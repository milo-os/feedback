package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostSort orders PostQuery results.
// +enum
type PostSort string

const (
	// PostSortTop sorts by votes.total, highest first.
	PostSortTop PostSort = "top"
	// PostSortNewest sorts by GitHub createdAt, newest first.
	PostSortNewest PostSort = "newest"
	// PostSortTrending sorts by votes.trendingScore.
	PostSortTrending PostSort = "trending"
	// PostSortRelevance is the default when text or similarTo is set.
	PostSortRelevance PostSort = "relevance"
	// PostSortRecentlyChanged sorts by the latest delivery history entry.
	PostSortRecentlyChanged PostSort = "recentlyChanged"
)

// ValidPostSorts lists every PostSort value.
var ValidPostSorts = []PostSort{
	PostSortTop,
	PostSortNewest,
	PostSortTrending,
	PostSortRelevance,
	PostSortRecentlyChanged,
}

// +genclient
// +genclient:nonNamespaced
// +genclient:onlyVerbs=create
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PostQuery searches, filters and sorts posts. Create it and read the results
// from status; it is not stored.
//
//	apiVersion: feedback.miloapis.com/v1alpha1
//	kind: PostQuery
//	spec:
//	  text: wildcard hostnames
//	  filter:
//	    state: [planning, on-deck, in-progress]
//	  sort: top
//	  limit: 20
type PostQuery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostQuerySpec   `json:"spec"`
	Status PostQueryStatus `json:"status,omitempty"`
}

// PostQuerySpec describes the query.
type PostQuerySpec struct {
	// Text is a keyword search, answered by the search service.
	// +optional
	Text string `json:"text,omitempty"`
	// SimilarTo is draft feedback text. Results are posts with similar
	// embeddings, for duplicate checks.
	// +optional
	SimilarTo string `json:"similarTo,omitempty"`
	// Filter narrows the results.
	// +optional
	Filter PostQueryFilter `json:"filter,omitempty"`
	// Sort orders the results. Defaults to relevance when text or similarTo is
	// set, otherwise top.
	// +optional
	Sort PostSort `json:"sort,omitempty"`
	// Limit is the page size.
	// +optional
	Limit int32 `json:"limit,omitempty"`
	// Continue is status.continue from the previous page.
	// +optional
	Continue string `json:"continue,omitempty"`
	// Facets names the fields to count values for: state, categories, orgs or
	// deliveryStage.
	// +optional
	// +listType=set
	Facets []string `json:"facets,omitempty"`
}

// PostQueryFilter narrows PostQuery results. Every set field must match.
type PostQueryFilter struct {
	// +optional
	// +listType=set
	State []PostState `json:"state,omitempty"`
	// +optional
	// +listType=set
	Categories []string `json:"categories,omitempty"`
	// Orgs filters by GitHub organization.
	// +optional
	// +listType=set
	Orgs []string `json:"orgs,omitempty"`
	// +optional
	// +listType=set
	DeliveryStage []DeliveryStage `json:"deliveryStage,omitempty"`
	// VotedByMe keeps posts the caller voted on. User context only.
	// +optional
	VotedByMe bool `json:"votedByMe,omitempty"`
	// FollowedByMe keeps posts the caller follows. User context only.
	// +optional
	FollowedByMe bool `json:"followedByMe,omitempty"`
}

// PostQueryStatus carries the results.
type PostQueryStatus struct {
	// +optional
	// +listType=atomic
	Results []PostQueryResult `json:"results,omitempty"`
	// Facets maps each requested field to value counts.
	// +optional
	Facets map[string]map[string]int32 `json:"facets,omitempty"`
	// Continue fetches the next page. Empty on the last page.
	// +optional
	Continue string `json:"continue,omitempty"`
}

// PostQueryResult is one post in the results.
type PostQueryResult struct {
	// Name is the Post name.
	Name  string `json:"name"`
	Title string `json:"title"`
	// +optional
	State PostState `json:"state,omitempty"`
	// +optional
	// +listType=set
	Categories []string `json:"categories,omitempty"`
	// +optional
	Votes PostVotes `json:"votes,omitempty"`
	// +optional
	DeliveryStage DeliveryStage `json:"deliveryStage,omitempty"`
	// VotedByMe is set in the user context.
	// +optional
	VotedByMe bool `json:"votedByMe,omitempty"`
	// Score is the relevance or similarity score, when text or similarTo is
	// used.
	// +optional
	Score *float64 `json:"score,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PostQueryList is required by the API machinery; PostQuery cannot be listed.
type PostQueryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PostQuery `json:"items"`
}
