package main

// ResultSet is a list of result rows
type ResultSet [][]interface{}

// ResultSetStats contains a result from a stats query
type ResultSetStats map[string][]*Filter

// Precompress compresses large strings in result set to allow faster updates (compressing would happen during locked update loop otherwise)
func (res *ResultSet) Precompress(offset int, columns *ColumnList) {
	for i := range *columns {
		col := (*columns)[i]
		if col.DataType == StringLargeCol {
			for j := range *res {
				(*res)[j][i+offset] = interface2stringlarge((*res)[j][i+offset])
			}
		}

	}
}
