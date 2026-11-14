package demo

import "time"

type Author struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Email     string    `gorm:"uniqueIndex;size:255;not null" json:"email"`
	Name      string    `gorm:"size:255;not null" json:"name"`
	Posts     []Post    `json:"posts,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Author) TableName() string { return "authors" }

type Post struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	AuthorID  int64     `gorm:"index;not null" json:"authorId"`
	Author    *Author   `json:"author,omitempty"`
	Title     string    `gorm:"size:255;not null" json:"title"`
	Body      string    `json:"body"`
	Views     int       `gorm:"not null;default:0" json:"views"`
	Published bool      `gorm:"not null;default:false" json:"published"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Post) TableName() string { return "posts" }
