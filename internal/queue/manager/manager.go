package manager

import (
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"gorm.io/gorm"
)

type manager struct {
	dao *dao.Query
	db  *gorm.DB
}
